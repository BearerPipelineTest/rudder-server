package manager

import (
	"context"

	"golang.org/x/sync/errgroup"

	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/router"
	"github.com/rudderlabs/rudder-server/router/batchrouter"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

var (
	objectStorageDestinations = []string{"S3", "GCS", "AZURE_BLOB", "MINIO", "DIGITAL_OCEAN_SPACES"}
	asyncDestinations         = []string{"MARKETO_BULK_UPLOAD"}
	warehouseDestinations     = []string{"RS", "BQ", "SNOWFLAKE", "POSTGRES", "CLICKHOUSE", "MSSQL", "AZURE_SYNAPSE", "S3_DATALAKE", "GCS_DATALAKE", "AZURE_DATALAKE", "DELTALAKE"}
	pkgLogger                 = logger.NewLogger().Child("router")
)

type LifecycleManager struct {
	rt            *router.Factory
	brt           *batchrouter.Factory
	BackendConfig backendconfig.BackendConfig
	currentCancel context.CancelFunc
	waitGroup     *errgroup.Group
}

func (*LifecycleManager) Run(ctx context.Context) error {
	return ctx.Err()
}

// Start starts a Router, this is not a blocking call.
// If the router is not completely started and the data started coming then also it will not be problematic as we
// are assuming that the DBs will be up.
func (r *LifecycleManager) Start() error {
	currentCtx, cancel := context.WithCancel(context.Background())
	r.currentCancel = cancel
	g, _ := errgroup.WithContext(context.Background())
	r.waitGroup = g
	g.Go(func() error {
		r.monitorDestRouters(currentCtx, *r.rt, *r.brt)
		return nil
	})
	return nil
}

// Stop stops the Router, this is a blocking call.
func (r *LifecycleManager) Stop() {
	r.currentCancel()
	_ = r.waitGroup.Wait()
}

// New creates a new Router instance
func New(rtFactory *router.Factory, brtFactory *batchrouter.Factory,
	backendConfig backendconfig.BackendConfig,
) *LifecycleManager {
	return &LifecycleManager{
		rt:            rtFactory,
		brt:           brtFactory,
		BackendConfig: backendConfig,
	}
}

// Gets the config from config backend and extracts enabled write-keys
func (r *LifecycleManager) monitorDestRouters(ctx context.Context, routerFactory router.Factory,
	batchrouterFactory batchrouter.Factory,
) {
	ch := r.BackendConfig.Subscribe(ctx, backendconfig.TopicBackendConfig)
	dstToRouter := make(map[string]*router.HandleT)
	dstToBatchRouter := make(map[string]*batchrouter.HandleT)
	cleanup := make([]func(), 0)

	// Crash recover routerDB, batchRouterDB
	// Note: The following cleanups can take time if there are too many
	// rt / batch_rt tables and there would be a delay reading from the 'ch' channel
	// However, this shouldn't be the problem since backend config pushes config
	// to its subscribers in separate goroutines to prevent blocking.
	routerFactory.RouterDB.DeleteExecuting()
	batchrouterFactory.RouterDB.DeleteExecuting()

loop:
	for {
		select {
		case <-ctx.Done():
			pkgLogger.Infof("Router monitor stopped Context Cancelled")
			break loop
		case config, open := <-ch:
			if !open {
				pkgLogger.Infof("Router monitor stopped, Config Channel Closed")
				break loop
			}
			sources := config.Data.(backendconfig.ConfigT)
			enabledDestinations := make(map[string]bool)
			for _, source := range sources.Sources {
				for _, destination := range source.Destinations { // TODO skipcq: CRT-P0006
					enabledDestinations[destination.DestinationDefinition.Name] = true
					// For batch router destinations
					if misc.ContainsString(objectStorageDestinations, destination.DestinationDefinition.Name) ||
						misc.ContainsString(warehouseDestinations, destination.DestinationDefinition.Name) ||
						misc.ContainsString(asyncDestinations, destination.DestinationDefinition.Name) {
						_, ok := dstToBatchRouter[destination.DestinationDefinition.Name]
						if !ok {
							pkgLogger.Infof("Starting a new Batch Destination Router: %s", destination.DestinationDefinition.Name)
							brt := batchrouterFactory.New(destination.DestinationDefinition.Name)
							brt.Start()
							cleanup = append(cleanup, brt.Shutdown)
							dstToBatchRouter[destination.DestinationDefinition.Name] = brt
						}
					} else {
						_, ok := dstToRouter[destination.DestinationDefinition.Name]
						if !ok {
							pkgLogger.Infof("Starting a new Destination: %s", destination.DestinationDefinition.Name)
							rt := routerFactory.New(destination.DestinationDefinition)
							rt.Start()
							cleanup = append(cleanup, rt.Shutdown)
							dstToRouter[destination.DestinationDefinition.Name] = rt
						}
					}
				}
			}
		}
	}

	g, _ := errgroup.WithContext(context.Background())
	for _, f := range cleanup {
		f := f
		g.Go(func() error {
			f()
			return nil
		})
	}
	_ = g.Wait()
}
