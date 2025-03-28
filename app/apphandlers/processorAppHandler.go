package apphandlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/rudderlabs/rudder-server/utils/httputil"
	"github.com/rudderlabs/rudder-server/utils/types/deployment"
	"github.com/rudderlabs/rudder-server/utils/types/servermode"

	"golang.org/x/sync/errgroup"

	"github.com/bugsnag/bugsnag-go/v2"
	"github.com/gorilla/mux"

	"github.com/rudderlabs/rudder-server/app"
	"github.com/rudderlabs/rudder-server/app/cluster"
	"github.com/rudderlabs/rudder-server/app/cluster/state"
	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	"github.com/rudderlabs/rudder-server/jobsdb/prebackup"
	proc "github.com/rudderlabs/rudder-server/processor"
	"github.com/rudderlabs/rudder-server/router"
	"github.com/rudderlabs/rudder-server/router/batchrouter"
	routerManager "github.com/rudderlabs/rudder-server/router/manager"
	"github.com/rudderlabs/rudder-server/services/db"
	destinationdebugger "github.com/rudderlabs/rudder-server/services/debugger/destination"
	transformationdebugger "github.com/rudderlabs/rudder-server/services/debugger/transformation"
	"github.com/rudderlabs/rudder-server/services/multitenant"
	"github.com/rudderlabs/rudder-server/services/transientsource"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/types"
)

// ProcessorApp is the type for Processor type implemention
type ProcessorApp struct {
	App            app.Interface
	VersionHandler func(w http.ResponseWriter, r *http.Request)
}

var (
	gatewayDB         *jobsdb.HandleT
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	webPort           int
	MaxHeaderBytes    int
)

func (processor *ProcessorApp) GetAppType() string {
	return fmt.Sprintf("rudder-server-%s", app.PROCESSOR)
}

func Init() {
	loadConfigHandler()
}

func loadConfigHandler() {
	config.RegisterDurationConfigVariable(0, &ReadTimeout, false, time.Second, []string{"ReadTimeout", "ReadTimeOutInSec"}...)
	config.RegisterDurationConfigVariable(0, &ReadHeaderTimeout, false, time.Second, []string{"ReadHeaderTimeout", "ReadHeaderTimeoutInSec"}...)
	config.RegisterDurationConfigVariable(10, &WriteTimeout, false, time.Second, []string{"WriteTimeout", "WriteTimeOutInSec"}...)
	config.RegisterDurationConfigVariable(720, &IdleTimeout, false, time.Second, []string{"IdleTimeout", "IdleTimeoutInSec"}...)
	config.RegisterIntConfigVariable(8086, &webPort, false, 1, "Processor.webPort")
	config.RegisterIntConfigVariable(524288, &MaxHeaderBytes, false, 1, "MaxHeaderBytes")
}

func (processor *ProcessorApp) StartRudderCore(ctx context.Context, options *app.Options) error {
	pkgLogger.Info("Processor starting")

	rudderCoreDBValidator()
	rudderCoreWorkSpaceTableSetup()
	rudderCoreNodeSetup()
	rudderCoreBaseSetup()
	g, ctx := errgroup.WithContext(ctx)

	deploymentType, err := deployment.GetFromEnv()
	if err != nil {
		return fmt.Errorf("failed to get deployment type: %w", err)
	}
	pkgLogger.Infof("Configured deployment type: %q", deploymentType)

	reporting := processor.App.Features().Reporting.Setup(backendconfig.DefaultBackendConfig)

	g.Go(misc.WithBugsnag(func() error {
		reporting.AddClient(ctx, types.Config{ConnInfo: jobsdb.GetConnectionString()})
		return nil
	}))

	pkgLogger.Info("Clearing DB ", options.ClearDB)

	transformationdebugger.Setup()
	destinationdebugger.Setup(backendconfig.DefaultBackendConfig)

	migrationMode := processor.App.Options().MigrationMode
	reportingI := processor.App.Features().Reporting.GetReportingInstance()
	transientSources := transientsource.NewService(ctx, backendconfig.DefaultBackendConfig)
	prebackupHandlers := []prebackup.Handler{
		prebackup.DropSourceIds(transientSources.SourceIdsSupplier()),
	}
	rsourcesService, err := NewRsourcesService(deploymentType)
	if err != nil {
		return err
	}

	// IMP NOTE: All the jobsdb setups must happen before migrator setup.
	gwDBForProcessor := jobsdb.NewForRead(
		"gw",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithMigrationMode(migrationMode),
		jobsdb.WithStatusHandler(),
		jobsdb.WithQueryFilterKeys(jobsdb.QueryFiltersT{}),
		jobsdb.WithPreBackupHandlers(prebackupHandlers),
	)
	defer gwDBForProcessor.Close()
	gatewayDB = gwDBForProcessor
	routerDB := jobsdb.NewForReadWrite(
		"rt",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithMigrationMode(migrationMode),
		jobsdb.WithStatusHandler(),
		jobsdb.WithQueryFilterKeys(router.QueryFilters),
		jobsdb.WithPreBackupHandlers(prebackupHandlers),
	)
	defer routerDB.Close()
	batchRouterDB := jobsdb.NewForReadWrite(
		"batch_rt",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithMigrationMode(migrationMode),
		jobsdb.WithStatusHandler(),
		jobsdb.WithQueryFilterKeys(batchrouter.QueryFilters),
		jobsdb.WithPreBackupHandlers(prebackupHandlers),
	)
	defer batchRouterDB.Close()
	errDB := jobsdb.NewForReadWrite(
		"proc_error",
		jobsdb.WithClearDB(options.ClearDB),
		jobsdb.WithMigrationMode(migrationMode),
		jobsdb.WithStatusHandler(),
		jobsdb.WithQueryFilterKeys(jobsdb.QueryFiltersT{}),
		jobsdb.WithPreBackupHandlers(prebackupHandlers),
	)
	var tenantRouterDB jobsdb.MultiTenantJobsDB
	var multitenantStats multitenant.MultiTenantI
	if misc.UseFairPickup() {
		tenantRouterDB = &jobsdb.MultiTenantHandleT{HandleT: routerDB}
		multitenantStats = multitenant.NewStats(map[string]jobsdb.MultiTenantJobsDB{
			"rt":       tenantRouterDB,
			"batch_rt": &jobsdb.MultiTenantLegacy{HandleT: batchRouterDB},
		})
	} else {
		tenantRouterDB = &jobsdb.MultiTenantLegacy{HandleT: routerDB}
		multitenantStats = multitenant.WithLegacyPickupJobs(multitenant.NewStats(map[string]jobsdb.MultiTenantJobsDB{
			"rt":       tenantRouterDB,
			"batch_rt": &jobsdb.MultiTenantLegacy{HandleT: batchRouterDB},
		}))
	}

	if processor.App.Features().Migrator != nil {
		if migrationMode == db.IMPORT || migrationMode == db.EXPORT || migrationMode == db.IMPORT_EXPORT {
			startProcessorFunc := func() {
				g.Go(func() error {
					clearDB := false
					if enableProcessor {
						return StartProcessor(
							ctx, &clearDB, gwDBForProcessor, routerDB, batchRouterDB, errDB,
							reportingI, multitenant.NOOP, transientSources, rsourcesService,
						)
					}
					return nil
				})
			}
			startRouterFunc := func() {
				if enableRouter {
					g.Go(func() error {
						StartRouter(ctx, tenantRouterDB, batchRouterDB, errDB, reportingI, multitenant.NOOP, transientSources, rsourcesService)
						return nil
					})
				}
			}
			enableRouter = false
			enableProcessor = false

			processor.App.Features().Migrator.PrepareJobsdbsForImport(nil, routerDB, batchRouterDB)
			g.Go(func() error {
				processor.App.Features().Migrator.Run(ctx, gwDBForProcessor, routerDB, batchRouterDB, startProcessorFunc, startRouterFunc) // TODO
				return nil
			})
		}
	}
	var modeProvider cluster.ChangeEventProvider

	switch deploymentType {
	case deployment.MultiTenantType:
		pkgLogger.Info("using ETCD Based Dynamic Cluster Manager")
		modeProvider = state.NewETCDDynamicProvider()
	case deployment.DedicatedType:
		// FIXME: hacky way to determine servermode
		pkgLogger.Info("using Static Cluster Manager")
		if enableProcessor && enableRouter {
			modeProvider = state.NewStaticProvider(servermode.NormalMode)
		} else {
			modeProvider = state.NewStaticProvider(servermode.DegradedMode)
		}
	default:
		return fmt.Errorf("unsupported deployment type: %q", deploymentType)
	}

	p := proc.New(ctx, &options.ClearDB, gwDBForProcessor, routerDB, batchRouterDB, errDB, multitenantStats, reportingI, transientSources, rsourcesService)

	rtFactory := &router.Factory{
		Reporting:        reportingI,
		Multitenant:      multitenantStats,
		BackendConfig:    backendconfig.DefaultBackendConfig,
		RouterDB:         tenantRouterDB,
		ProcErrorDB:      errDB,
		TransientSources: transientSources,
		RsourcesService:  rsourcesService,
	}
	brtFactory := &batchrouter.Factory{
		Reporting:        reportingI,
		Multitenant:      multitenantStats,
		BackendConfig:    backendconfig.DefaultBackendConfig,
		RouterDB:         batchRouterDB,
		ProcErrorDB:      errDB,
		TransientSources: transientSources,
		RsourcesService:  rsourcesService,
	}
	rt := routerManager.New(rtFactory, brtFactory, backendconfig.DefaultBackendConfig)

	dm := cluster.Dynamic{
		Provider:         modeProvider,
		GatewayComponent: false,
		GatewayDB:        gwDBForProcessor,
		RouterDB:         routerDB,
		BatchRouterDB:    batchRouterDB,
		ErrorDB:          errDB,
		Processor:        p,
		Router:           rt,
		MultiTenantStat:  multitenantStats,
	}

	g.Go(func() error {
		return startHealthWebHandler(ctx)
	})

	g.Go(func() error {
		// This should happen only after setupDatabaseTables() is called and journal table migrations are done
		// because if this start before that then there might be a case when ReadDB will try to read the owner table
		// which gets created after either Write or ReadWrite DB is created.
		return dm.Run(ctx)
	})

	g.Go(func() error {
		return rsourcesService.CleanupLoop(ctx)
	})

	return g.Wait()
}

func (processor *ProcessorApp) HandleRecovery(options *app.Options) {
	db.HandleNullRecovery(options.NormalMode, options.DegradedMode, options.MigrationMode, misc.AppStartTime, app.PROCESSOR)
}

func startHealthWebHandler(ctx context.Context) error {
	// Port where Processor health handler is running
	pkgLogger.Infof("Starting in %d", webPort)
	srvMux := mux.NewRouter()
	srvMux.HandleFunc("/health", app.LivenessHandler(gatewayDB))
	srvMux.HandleFunc("/", app.LivenessHandler(gatewayDB))
	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(webPort),
		Handler:           bugsnag.Handler(srvMux),
		ReadTimeout:       ReadTimeout,
		ReadHeaderTimeout: ReadHeaderTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		MaxHeaderBytes:    MaxHeaderBytes,
	}

	return httputil.ListenAndServe(ctx, srv)
}
