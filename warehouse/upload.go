package warehouse

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/rruntime"
	"github.com/rudderlabs/rudder-server/services/pgnotifier"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"github.com/rudderlabs/rudder-server/utils/timeutil"
	"github.com/rudderlabs/rudder-server/warehouse/identity"
	"github.com/rudderlabs/rudder-server/warehouse/manager"
	warehouseutils "github.com/rudderlabs/rudder-server/warehouse/utils"
)

const (
	WaitingState                            = "waiting"
	ExecutingState                          = "executing"
	GeneratingLoadFileState                 = "generating_load_file"
	GeneratingLoadFileFailedState           = "generating_load_file_failed"
	GeneratedLoadFileState                  = "generated_load_file"
	UpdatingSchemaState                     = "updating_schema"
	UpdatingSchemaFailedState               = "updating_schema_failed"
	UpdatedSchemaState                      = "updated_schema"
	ExportingDataState                      = "exporting_data"
	ExportingDataFailedState                = "exporting_data_failed"
	ExportedDataState                       = "exported_data"
	AbortedState                            = "aborted"
	GeneratingStagingFileFailedState        = "generating_staging_file_failed"
	GeneratedStagingFileState               = "generated_staging_file"
	FetchingSchemaState                     = "fetching_schema"
	FetchingSchemaFailedState               = "fetching_schema_failed"
	PopulatingHistoricIdentitiesState       = "populating_historic_identities"
	PopulatingHistoricIdentitiesStateFailed = "populating_historic_identities_failed"
	ConnectFailedState                      = "connect_failed"
)

type UploadT struct {
	ID                 int64
	Namespace          string
	SourceID           string
	DestinationID      string
	DestinationType    string
	StartStagingFileID int64
	EndStagingFileID   int64
	StartLoadFileID    int64
	EndLoadFileID      int64
	Status             string
	Schema             warehouseutils.SchemaT
	Error              json.RawMessage
	Timings            []map[string]string
	FirstAttemptAt     time.Time
	LastAttemptAt      time.Time
	Attempts           int64
}

type UploadJobT struct {
	upload       *UploadT
	dbHandle     *sql.DB
	warehouse    warehouseutils.WarehouseT
	whManager    manager.ManagerI
	stagingFiles []*StagingFileT
	pgNotifier   *pgnotifier.PgNotifierT
	schemaHandle *SchemaHandleT
}

type UploadColumnT struct {
	Column string
	Value  interface{}
}

const (
	UploadStatusField          = "status"
	UploadStartLoadFileIDField = "start_load_file_id"
	UploadEndLoadFileIDField   = "end_load_file_id"
	UploadUpdatedAtField       = "updated_at"
	UploadTimingsField         = "timings"
	UploadSchemaField          = "schema"
	UploadLastExecAtField      = "last_exec_at"
)

var maxParallelLoads map[string]int

func init() {
	setMaxParallelLoads()
}

func setMaxParallelLoads() {
	maxParallelLoads = map[string]int{
		"BQ":         config.GetInt("Warehouse.bigquery.maxParallelLoads", 20),
		"RS":         config.GetInt("Warehouse.redshift.maxParallelLoads", 3),
		"POSTGRES":   config.GetInt("Warehouse.postgres.maxParallelLoads", 3),
		"SNOWFLAKE":  config.GetInt("Warehouse.snowflake.maxParallelLoads", 3),
		"CLICKHOUSE": config.GetInt("Warehouse.clickhouse.maxParallelLoads", 3),
	}
}

func (job *UploadJobT) identifiesTableName() string {
	return warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.IdentifiesTable)
}

func (job *UploadJobT) usersTableName() string {
	return warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.UsersTable)
}

func (job *UploadJobT) identityMergeRulesTableName() string {
	return warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.IdentityMergeRulesTable)
}

func (job *UploadJobT) identityMappingsTableName() string {
	return warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.IdentityMappingsTable)
}

func (job *UploadJobT) trackLongRunningUpload() chan struct{} {
	ch := make(chan struct{}, 1)
	rruntime.Go(func() {
		select {
		case _ = <-ch:
			// do nothing
		case <-time.After(longRunningUploadStatThresholdInMin):
			logger.Infof("[WH]: Registering stat for long running upload: %d, dest: %s:%s:%s", job.upload.ID, job.warehouse.Type, job.warehouse.Namespace, job.upload.DestinationID)
			warehouseutils.DestStat(stats.CountType, "long_running_upload", job.warehouse.Destination.ID).Count(1)
		}
	})
	return ch
}

func (job *UploadJobT) run() (err error) {
	ch := job.trackLongRunningUpload()
	defer func() {
		ch <- struct{}{}
	}()

	// START: processing of upload job
	if len(job.stagingFiles) == 0 {
		err := fmt.Errorf("No staging files found")
		job.setUploadError(err, GeneratingLoadFileFailedState)
		return err
	}

	err = job.setUploadStatus(FetchingSchemaState)
	if err != nil {
		return err
	}

	schemaHandle := SchemaHandleT{
		warehouse:    job.warehouse,
		stagingFiles: job.stagingFiles,
		dbHandle:     job.dbHandle,
	}
	job.schemaHandle = &schemaHandle
	schemaHandle.localSchema = schemaHandle.getLocalSchema()
	schemaHandle.schemaInWarehouse, err = schemaHandle.fetchSchemaFromWarehouse()
	if err != nil {
		job.setUploadError(err, FetchingSchemaFailedState)
		return err
	}

	hasSchemaChanged := !compareSchema(schemaHandle.localSchema, schemaHandle.schemaInWarehouse)
	if hasSchemaChanged {
		schemaHandle.updateLocalSchema(schemaHandle.schemaInWarehouse)
		schemaHandle.localSchema = schemaHandle.schemaInWarehouse
	}

	// consolidate schemas over all staging files and set consolidatedSchema for upload
	// only set if upload does not have schema(to skip reconsolidating on upload retry)
	// recompute if schema in warehouse has changed from one in wh_schemas
	if len(job.upload.Schema) == 0 || hasSchemaChanged {
		// merge schemas over all staging files in this batch
		logger.Infof("[WH]: Consolidating all staging files schema's with schema in wh_schemas...")
		schemaHandle.uploadSchema = schemaHandle.consolidateStagingFilesSchemaUsingWarehouseSchema()

		// TODO: remove err?
		err = job.setSchema(schemaHandle.uploadSchema)
		if err != nil {
			job.setUploadError(err, UpdatingSchemaFailedState)
			return err
		}
	} else {
		schemaHandle.uploadSchema = job.upload.Schema
	}

	// create entries in wh_table_uploads for all tables in uploadSchema
	if !job.areTableUploadsCreated() {
		err := job.initTableUploads()
		if err != nil {
			// TODO: Handle error / Retry
			logger.Error("[WH]: Error creating records in wh_table_uploads", err)
		}
	}

	// TODO: rename metric
	createPlusUploadTimer := warehouseutils.DestStat(stats.TimerType, "stagingfileset_total_handling_time", job.upload.DestinationID)
	createPlusUploadTimer.Start()

	// generate load files only if not done before
	// upload records have start_load_file_id and end_load_file_id set to 0 on creation
	// and are updated on creation of load files
	logger.Infof("[WH]: Processing %d staging files in upload job:%v with staging files from %v to %v for %s:%s", len(job.stagingFiles), job.upload.ID, job.stagingFiles[0].ID, job.stagingFiles[len(job.stagingFiles)-1].ID, job.warehouse.Type, job.upload.DestinationID)
	err = job.setUploadColumns(
		UploadColumnT{Column: UploadLastExecAtField, Value: timeutil.Now()},
	)
	if err != nil {
		return err
	}

	// generate load files
	// skip if already generated (valid entries in StartLoadFileID and EndLoadFileID)
	// regenerate if schema in warehouse has changed from one in wh_schemas to avoid incorrect setting of fields/discards
	if job.upload.StartLoadFileID == 0 || hasSchemaChanged {
		logger.Infof("[WH]: Starting load file generation for warehouse destination %s:%s upload:%d", job.warehouse.Type, job.warehouse.Destination.Name, job.upload.ID)
		err = job.setUploadStatus(GeneratingLoadFileState)
		if err != nil {
			return err
		}
		var loadFileIDs []int64
		loadFileIDs, err = job.createLoadFiles()
		if err != nil {
			//Unreachable code. So not modifying the stat 'failed_uploads', which is reused later for copying.
			job.setUploadError(err, GeneratingLoadFileFailedState)
			job.setStagingFilesStatus(warehouseutils.StagingFileFailedState, err)
			warehouseutils.DestStat(stats.CountType, "process_staging_files_failures", job.warehouse.Destination.ID).Count(len(job.stagingFiles))
			return err
		}
		job.setStagingFilesStatus(warehouseutils.StagingFileSucceededState, nil)
		warehouseutils.DestStat(stats.CountType, "process_staging_files_success", job.warehouse.Destination.ID).Count(len(job.stagingFiles))
		warehouseutils.DestStat(stats.CountType, "generate_load_files", job.warehouse.Destination.ID).Count(len(loadFileIDs))

		startLoadFileID := loadFileIDs[0]
		endLoadFileID := loadFileIDs[len(loadFileIDs)-1]

		err = job.setUploadStatus(
			GeneratedLoadFileState,
			UploadColumnT{Column: UploadStartLoadFileIDField, Value: startLoadFileID},
			UploadColumnT{Column: UploadEndLoadFileIDField, Value: endLoadFileID},
		)
		if err != nil {
			return err
		}

		job.upload.StartLoadFileID = startLoadFileID
		job.upload.EndLoadFileID = endLoadFileID
		job.upload.Status = GeneratedLoadFileState

		for tableName := range job.upload.Schema {
			err := job.updateTableEventsCount(tableName)
			if err != nil {
				return err
			}
		}
	}

	whManager := job.whManager

	err = whManager.Setup(job.warehouse, job)
	if err != nil {
		job.setUploadError(err, ConnectFailedState)
		return err
	}
	defer whManager.Cleanup()

	// get total events in each table and overall to be used to record metrics
	totalEventsPerTableMap := job.getEventsPerTableUpload()
	var totalEventsInUpload int
	for _, count := range totalEventsPerTableMap {
		totalEventsInUpload += count
	}

	// start: update schema in warehouse
	diff := getSchemaDiff(schemaHandle.schemaInWarehouse, job.upload.Schema)
	if diff.Exists {
		err = job.setUploadStatus(UpdatingSchemaState)
		if err != nil {
			return err
		}
		timer := warehouseutils.DestStat(stats.TimerType, "migrate_schema_time", job.warehouse.Destination.ID)
		timer.Start()
		logger.Infof("[WH]: Starting migration in warehouse destination %s:%s upload:%d", job.warehouse.Type, job.warehouse.Destination.Name, job.upload.ID)
		err = whManager.MigrateSchema(diff)
		if err != nil {
			state, _ := job.setUploadError(err, UpdatingSchemaFailedState)
			if state == AbortedState {
				warehouseutils.DestStat(stats.CountType, "total_records_upload_failed", job.warehouse.Destination.ID).Count(totalEventsInUpload)
			}
			return err
		}
		timer.End()

		// update all schemas in handle to the updated version from warehouse after successful migration
		schemaHandle.updateLocalSchema(diff.UpdatedSchema)
		schemaHandle.localSchema = schemaHandle.schemaAfterUpload
		schemaHandle.schemaAfterUpload = diff.UpdatedSchema
		schemaHandle.schemaInWarehouse = schemaHandle.schemaAfterUpload
		job.setUploadStatus(UpdatedSchemaState)
	} else {
		// no alter done to schema in this upload
		schemaHandle.schemaAfterUpload = schemaHandle.schemaInWarehouse
	}
	// end: update schema in warehouse

	logger.Infof("[WH]: Starting data copy to warehouse destination %s:%s upload:%d", job.warehouse.Type, job.warehouse.Destination.Name, job.upload.ID)
	job.setUploadStatus(ExportingDataState)
	var loadErrors []error
	uploadSchema := job.upload.Schema

	// metric for total copy time of all tables data to warehouse
	timer := warehouseutils.DestStat(stats.TimerType, "upload_time", job.warehouse.Destination.ID)
	timer.Start()

	userTables := []string{job.identifiesTableName(), job.usersTableName()}
	if _, ok := uploadSchema[job.identifiesTableName()]; ok {
		if !job.shouldTableBeLoaded(job.identifiesTableName()) && !job.shouldTableBeLoaded(job.usersTableName()) {
			// do nothing as both tables are loaded
		} else {
			logger.Infof(`[WH]: Starting load for user tables in namespace %s of destination %s:%s`, job.warehouse.Namespace, job.warehouse.Type, job.warehouse.Destination.ID)
			for _, tName := range userTables {
				job.setTableUploadStatus(tName, ExecutingState)
			}
			errorMap := whManager.LoadUserTables()
			loadErrors = append(loadErrors, job.setTableStatusFromErrorMap(errorMap)...)
			if len(loadErrors) == 0 {
				for tName := range errorMap {
					job.recordMetric(tName, totalEventsPerTableMap)
				}
			}
		}
	}

	if warehouseutils.IDResolutionEnabled() && misc.ContainsString(warehouseutils.IdentityEnabledWarehouses, job.warehouse.Type) {
		if _, ok := uploadSchema[job.identityMergeRulesTableName()]; ok {
			errorMap := job.loadIdentityTables(false)
			loadErrors = append(loadErrors, job.setTableStatusFromErrorMap(errorMap)...)
		}
	}

	var parallelLoads int
	var ok bool
	if parallelLoads, ok = maxParallelLoads[job.warehouse.Type]; !ok {
		parallelLoads = 1
	}

	var wg sync.WaitGroup
	wg.Add(len(uploadSchema))
	loadChan := make(chan struct{}, parallelLoads)
	skipPrevLoadedTableNames := []string{job.identifiesTableName(), job.usersTableName(), job.identityMergeRulesTableName(), job.identityMappingsTableName()}
	for tableName := range uploadSchema {
		if misc.ContainsString(skipPrevLoadedTableNames, tableName) {
			wg.Done()
			continue
		}
		if !job.shouldTableBeLoaded(tableName) {
			wg.Done()
			continue
		}

		tName := tableName
		loadChan <- struct{}{}
		rruntime.Go(func() {
			logger.Infof(`[WH]: Starting load for table %s in namespace %s of destination %s:%s`, tName, job.warehouse.Namespace, job.warehouse.Type, job.warehouse.Destination.ID)
			job.setTableUploadStatus(tName, ExecutingState)
			err := whManager.LoadTable(tName)
			// TODO: set wh_table_uploads error
			if err != nil {
				loadErrors = append(loadErrors, err)
				job.setTableUploadError(tName, ExportingDataFailedState, err)
			} else {
				job.setTableUploadStatus(tName, ExportedDataState)
				job.recordMetric(tName, totalEventsPerTableMap)
			}
			wg.Done()
			<-loadChan
		})
	}
	wg.Wait()
	timer.End()

	if len(loadErrors) > 0 {
		state, _ := job.setUploadError(warehouseutils.ConcatErrors(loadErrors), ExportingDataFailedState)
		if state == AbortedState {
			uploadedEvents := job.getTotalEventsUploaded()
			warehouseutils.DestStat(stats.CountType, "total_records_uploaded", job.warehouse.Destination.ID).Count(uploadedEvents)
			warehouseutils.DestStat(stats.CountType, "total_records_upload_failed", job.warehouse.Destination.ID).Count(totalEventsInUpload - uploadedEvents)
		}
		return
	}
	job.setUploadStatus(ExportedDataState)
	warehouseutils.DestStat(stats.CountType, "total_records_uploaded", job.warehouse.Destination.ID).Count(totalEventsInUpload)

	return nil
}

func (job *UploadJobT) resolveIdentities(populateHistoricIdentities bool) (err error) {
	idr := identity.HandleT{
		Warehouse:        job.warehouse,
		DbHandle:         job.dbHandle,
		UploadID:         job.upload.ID,
		Uploader:         job,
		WarehouseManager: job.whManager,
	}
	if populateHistoricIdentities {
		return idr.ResolveHistoricIdentities()
	}
	return idr.Resolve()
}

func (job *UploadJobT) loadIdentityTables(populateHistoricIdentities bool) (errorMap map[string]error) {
	logger.Infof(`[WH]: Starting load for identity tables in namespace %s of destination %s:%s`, job.warehouse.Namespace, job.warehouse.Type, job.warehouse.Destination.ID)
	errorMap = make(map[string]error)
	// var generated bool
	if generated, err := job.areIdentityTablesLoadFilesGenerated(); !generated {
		err = job.resolveIdentities(populateHistoricIdentities)
		if err != nil {
			logger.Errorf(`SF: ID Resolution operation failed: %v`, err)
			errorMap[job.identityMergeRulesTableName()] = err
			return errorMap
		}
	}

	if !job.hasBeenLoaded(job.identityMergeRulesTableName()) {
		errorMap[job.identityMergeRulesTableName()] = nil
		job.setTableUploadStatus(job.identityMergeRulesTableName(), ExecutingState)
		err := job.whManager.LoadIdentityMergeRulesTable()
		if err != nil {
			errorMap[job.identityMergeRulesTableName()] = err
			return errorMap
		}
	}

	if !job.hasBeenLoaded(job.identityMappingsTableName()) {
		errorMap[job.identityMappingsTableName()] = nil
		job.setTableUploadStatus(job.identityMappingsTableName(), ExecutingState)
		err := job.whManager.LoadIdentityMappingsTable()
		if err != nil {
			errorMap[job.identityMappingsTableName()] = nil
			return errorMap
		}
	}
	return errorMap
}

func (job *UploadJobT) setTableStatusFromErrorMap(errorMap map[string]error) (errors []error) {
	for tName, err := range errorMap {
		// TODO: set last_exec_time
		if err != nil {
			errors = append(errors, err)
			job.setTableUploadError(tName, ExportingDataFailedState, err)
		} else {
			job.setTableUploadStatus(tName, ExportedDataState)
		}
	}
	return errors
}

func (job *UploadJobT) recordMetric(tableName string, totalEventsPerTableMap map[string]int) {
	// add metric to record total loaded rows to standard tables
	// adding metric for all event tables might result in too many metrics
	tablesToRecordEventsMetric := []string{"tracks", "users", "identifies", "pages", "screens", "aliases", "groups", "rudder_discards"}
	if misc.Contains(tablesToRecordEventsMetric, strings.ToLower(tableName)) {
		if eventsInTable, ok := totalEventsPerTableMap[tableName]; ok {
			warehouseutils.DestStat(stats.CountType, fmt.Sprintf(`%s_table_records_uploaded`, strings.ToLower(tableName)), job.warehouse.Destination.ID).Count(eventsInTable)
		}
	}
}

// getUploadTimings returns timings json column
// eg. timings: [{exporting_data: 2020-04-21 15:16:19.687716, exported_data: 2020-04-21 15:26:34.344356}]
func (job *UploadJobT) getUploadTimings() (timings []map[string]string) {
	var rawJSON json.RawMessage
	sqlStatement := fmt.Sprintf(`SELECT timings FROM %s WHERE id=%d`, warehouseutils.WarehouseUploadsTable, job.upload.ID)
	err := job.dbHandle.QueryRow(sqlStatement).Scan(&rawJSON)
	if err != nil {
		return
	}
	err = json.Unmarshal(rawJSON, &timings)
	return
}

// getNewTimings appends current status with current time to timings column
// eg. status: exported_data, timings: [{exporting_data: 2020-04-21 15:16:19.687716] -> [{exporting_data: 2020-04-21 15:16:19.687716, exported_data: 2020-04-21 15:26:34.344356}]
func (job *UploadJobT) getNewTimings(status string) []byte {
	timings := job.getUploadTimings()
	timing := map[string]string{status: timeutil.Now().Format(misc.RFC3339Milli)}
	timings = append(timings, timing)
	marshalledTimings, err := json.Marshal(timings)
	if err != nil {
		panic(err)
	}
	return marshalledTimings
}

func (job *UploadJobT) getUploadFirstAttemptTime() (timing time.Time) {
	var firstTiming sql.NullString
	sqlStatement := fmt.Sprintf(`SELECT timings->0 as firstTimingObj FROM %s WHERE id=%d`, warehouseutils.WarehouseUploadsTable, job.upload.ID)
	err := job.dbHandle.QueryRow(sqlStatement).Scan(&firstTiming)
	if err != nil {
		return
	}
	_, timing = warehouseutils.TimingFromJSONString(firstTiming)
	return timing
}

func (job *UploadJobT) setUploadStatus(status string, additionalFields ...UploadColumnT) (err error) {
	logger.Infof("[WH]: Setting status of %s for wh_upload:%v", status, job.upload.ID)
	marshalledTimings := job.getNewTimings(status)
	opts := []UploadColumnT{
		{Column: UploadStatusField, Value: status},
		{Column: UploadTimingsField, Value: marshalledTimings},
		{Column: UploadUpdatedAtField, Value: timeutil.Now()},
	}

	additionalFields = append(additionalFields, opts...)

	return job.setUploadColumns(
		additionalFields...,
	)
}

// SetSchema
func (job *UploadJobT) setSchema(consolidatedSchema warehouseutils.SchemaT) error {
	marshalledSchema, err := json.Marshal(consolidatedSchema)
	if err != nil {
		panic(err)
	}
	job.upload.Schema = consolidatedSchema
	return job.setUploadColumns(
		UploadColumnT{Column: UploadSchemaField, Value: marshalledSchema},
	)
}

// SetUploadColumns sets any column values passed as args in UploadColumnT format for WarehouseUploadsTable
func (job *UploadJobT) setUploadColumns(fields ...UploadColumnT) (err error) {
	var columns string
	values := []interface{}{job.upload.ID}
	// setting values using syntax $n since Exec can correctly format time.Time strings
	for idx, f := range fields {
		// start with $2 as $1 is upload.ID
		columns += fmt.Sprintf(`%s=$%d`, f.Column, idx+2)
		if idx < len(fields)-1 {
			columns += ","
		}
		values = append(values, f.Value)
	}
	sqlStatement := fmt.Sprintf(`UPDATE %s SET %s WHERE id=$1`, warehouseutils.WarehouseUploadsTable, columns)
	_, err = dbHandle.Exec(sqlStatement, values...)

	return err
}

func (job *UploadJobT) setUploadError(statusError error, state string) (newstate string, err error) {
	logger.Errorf("[WH]: Failed during %s stage: %v\n", state, statusError.Error())
	upload := job.upload

	job.setUploadStatus(state)
	var e map[string]map[string]interface{}
	json.Unmarshal(job.upload.Error, &e)
	if e == nil {
		e = make(map[string]map[string]interface{})
	}
	if _, ok := e[state]; !ok {
		e[state] = make(map[string]interface{})
	}
	errorByState := e[state]
	// increment attempts for errored stage
	if attempt, ok := errorByState["attempt"]; ok {
		errorByState["attempt"] = int(attempt.(float64)) + 1
	} else {
		errorByState["attempt"] = 1
	}
	// append errors for errored stage
	if errList, ok := errorByState["errors"]; ok {
		errorByState["errors"] = append(errList.([]interface{}), statusError.Error())
	} else {
		errorByState["errors"] = []string{statusError.Error()}
	}
	// abort after configured retry attempts
	if errorByState["attempt"].(int) > minRetryAttempts {
		firstTiming := job.getUploadFirstAttemptTime()
		if !firstTiming.IsZero() && (timeutil.Now().Sub(firstTiming) > retryTimeWindow) {
			state = AbortedState
		}
	}
	serializedErr, _ := json.Marshal(&e)
	sqlStatement := fmt.Sprintf(`UPDATE %s SET status=$1, error=$2, updated_at=$3 WHERE id=$4`, warehouseutils.WarehouseUploadsTable)
	_, err = job.dbHandle.Exec(sqlStatement, state, serializedErr, timeutil.Now(), upload.ID)

	return state, err
}

func (job *UploadJobT) setStagingFilesStatus(status string, statusError error) (err error) {
	var ids []int64
	for _, stagingFile := range job.stagingFiles {
		ids = append(ids, stagingFile.ID)
	}
	// TODO: json.Marshal error instead of quoteliteral
	if statusError == nil {
		statusError = fmt.Errorf("{}")
	}
	sqlStatement := fmt.Sprintf(`UPDATE %s SET status=$1, error=$2, updated_at=$3 WHERE id=ANY($4)`, warehouseutils.WarehouseStagingFilesTable)
	_, err = dbHandle.Exec(sqlStatement, status, misc.QuoteLiteral(statusError.Error()), timeutil.Now(), pq.Array(ids))
	if err != nil {
		panic(err)
	}
	return
}

func (job *UploadJobT) setStagingFilesError(ids []int64, status string, statusError error) (err error) {
	logger.Errorf("[WH]: Failed processing staging files: %v", statusError.Error())
	sqlStatement := fmt.Sprintf(`UPDATE %s SET status=$1, error=$2, updated_at=$3 WHERE id=ANY($4)`, warehouseutils.WarehouseStagingFilesTable)
	_, err = job.dbHandle.Exec(sqlStatement, status, misc.QuoteLiteral(statusError.Error()), timeutil.Now(), pq.Array(ids))
	if err != nil {
		panic(err)
	}
	return
}

func (job *UploadJobT) areTableUploadsCreated() bool {
	sqlStatement := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE wh_upload_id=%d`, warehouseutils.WarehouseTableUploadsTable, job.upload.ID)
	var count int
	err := job.dbHandle.QueryRow(sqlStatement).Scan(&count)
	if err != nil {
		panic(err)
	}
	return count > 0
}

func (job *UploadJobT) initTableUploads() (err error) {

	//Using transactions for bulk copying
	txn, err := job.dbHandle.Begin()
	if err != nil {
		return
	}

	stmt, err := txn.Prepare(pq.CopyIn(warehouseutils.WarehouseTableUploadsTable, "wh_upload_id", "table_name", "status", "error", "created_at", "updated_at"))
	if err != nil {
		return
	}

	destType := job.warehouse.Type
	schemaForUpload := job.upload.Schema
	tables := make([]string, 0, len(schemaForUpload))
	for t := range schemaForUpload {
		tables = append(tables, t)
		// also track upload to rudder_identity_mappings if the upload has records for rudder_identity_merge_rules
		if misc.ContainsString(warehouseutils.IdentityEnabledWarehouses, destType) && t == warehouseutils.ToProviderCase(destType, warehouseutils.IdentityMergeRulesTable) {
			if _, ok := schemaForUpload[warehouseutils.ToProviderCase(destType, warehouseutils.IdentityMappingsTable)]; !ok {
				tables = append(tables, warehouseutils.ToProviderCase(destType, warehouseutils.IdentityMappingsTable))
			}
		}
	}

	now := timeutil.Now()
	for _, table := range tables {
		_, err = stmt.Exec(job.upload.ID, table, "waiting", "{}", now, now)
		if err != nil {
			return
		}
	}

	_, err = stmt.Exec()
	if err != nil {
		return
	}

	err = stmt.Close()
	if err != nil {
		return
	}

	err = txn.Commit()
	if err != nil {
		return
	}
	return
}

func (job *UploadJobT) getTableUploadStatus(tableName string) (status string, err error) {
	sqlStatement := fmt.Sprintf(`SELECT status from %s WHERE wh_upload_id=%d AND table_name='%s' ORDER BY id DESC`, warehouseutils.WarehouseTableUploadsTable, job.upload.ID, tableName)
	err = dbHandle.QueryRow(sqlStatement).Scan(&status)
	return
}

func (job *UploadJobT) hasLoadFiles(tableName string) bool {
	sourceID := job.warehouse.Source.ID
	destID := job.warehouse.Destination.ID

	sqlStatement := fmt.Sprintf(`SELECT count(*) FROM %[1]s
								WHERE ( %[1]s.source_id='%[2]s' AND %[1]s.destination_id='%[3]s' AND %[1]s.table_name='%[4]s' AND %[1]s.id >= %[5]v AND %[1]s.id <= %[6]v)`,
		warehouseutils.WarehouseLoadFilesTable, sourceID, destID, tableName, job.upload.StartLoadFileID, job.upload.EndLoadFileID)
	var count int64
	err := dbHandle.QueryRow(sqlStatement).Scan(&count)
	if err != nil {
		panic(err)
	}
	return count > 0
}

func (job *UploadJobT) hasBeenLoaded(tableName string) bool {
	status, _ := job.getTableUploadStatus(tableName)
	if status == ExportedDataState {
		return true
	}
	return false
}

func (job *UploadJobT) shouldTableBeLoaded(tableName string) bool {
	status, _ := job.getTableUploadStatus(tableName)
	if status == ExportedDataState {
		return false
	}
	if !job.hasLoadFiles(tableName) {
		return false
	}
	return true
}

func (job *UploadJobT) getEventsPerTableUpload() map[string]int {
	eventsPerTableMap := make(map[string]int)

	sqlStatement := fmt.Sprintf(`select table_name, total_events from wh_table_uploads where wh_upload_id=%d and total_events > 0`, job.upload.ID)
	rows, err := job.dbHandle.Query(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	for rows.Next() {
		var tName string
		var totalEvents int
		err := rows.Scan(&tName, &totalEvents)
		if err != nil {
			panic(err)
		}
		eventsPerTableMap[tName] = totalEvents
	}
	return eventsPerTableMap
}

func (job *UploadJobT) getTotalEventsUploaded() (total int) {
	sqlStatement := fmt.Sprintf(`select sum(total_events) from wh_table_uploads where wh_upload_id=%d and status='%s'`, job.upload.ID, ExportedDataState)
	err := job.dbHandle.QueryRow(sqlStatement).Scan(&total)
	if err != nil {
		logger.Errorf(`[WH]: Failed to query wh_table_uploads: %w`, err)
	}
	return
}

func (job *UploadJobT) setTableUploadStatus(tableName string, status string) (err error) {
	// set last_exec_time only if status is executing
	execValues := []interface{}{status, timeutil.Now(), job.upload.ID, tableName}
	var lastExec string
	if status == ExecutingState {
		// setting values using syntax $n since Exec can correctlt format time.Time strings
		lastExec = fmt.Sprintf(`, last_exec_time=$%d`, len(execValues)+1)
		execValues = append(execValues, timeutil.Now())
	}
	sqlStatement := fmt.Sprintf(`UPDATE %s SET status=$1, updated_at=$2 %s WHERE wh_upload_id=$3 AND table_name=$4`, warehouseutils.WarehouseTableUploadsTable, lastExec)
	logger.Debugf("[WH]: Setting table upload status: %v", sqlStatement)
	_, err = dbHandle.Exec(sqlStatement, execValues...)
	if err != nil {
		panic(err)
	}
	return
}

func (job *UploadJobT) setTableUploadError(tableName string, status string, statusError error) (err error) {
	logger.Errorf("[WH]: Failed uploading table-%s for upload-%v: %v", tableName, job.upload.ID, statusError.Error())
	sqlStatement := fmt.Sprintf(`UPDATE %s SET status=$1, updated_at=$2, error=$3 WHERE wh_upload_id=$4 AND table_name=$5`, warehouseutils.WarehouseTableUploadsTable)
	logger.Debugf("[WH]: Setting table upload error: %v", sqlStatement)
	_, err = dbHandle.Exec(sqlStatement, status, timeutil.Now(), misc.QuoteLiteral(statusError.Error()), job.upload.ID, tableName)
	if err != nil {
		panic(err)
	}
	return
}

func (job *UploadJobT) createLoadFiles() (loadFileIDs []int64, err error) {
	destID := job.upload.DestinationID
	destType := job.upload.DestinationType
	stagingFiles := job.stagingFiles

	// stat for time taken to process staging files in a single job
	timer := warehouseutils.DestStat(stats.TimerType, "process_staging_files_batch_time", destID)
	timer.Start()

	job.setStagingFilesStatus(warehouseutils.StagingFileExecutingState, nil)

	logger.Debugf("[WH]: Starting batch processing %v stage files with %v workers for %s:%s", len(stagingFiles), noOfWorkers, destType, destID)
	var messages []pgnotifier.MessageT
	for _, stagingFile := range stagingFiles {
		payload := PayloadT{
			UploadID:            job.upload.ID,
			StagingFileID:       stagingFile.ID,
			StagingFileLocation: stagingFile.Location,
			Schema:              job.upload.Schema,
			SourceID:            job.warehouse.Source.ID,
			DestinationID:       destID,
			DestinationType:     destType,
			DestinationConfig:   job.warehouse.Destination.Config,
		}

		payloadJSON, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}
		message := pgnotifier.MessageT{
			Payload: payloadJSON,
		}
		messages = append(messages, message)
	}

	logger.Infof("[WH]: Publishing %d staging files to PgNotifier", len(messages))
	// var loadFileIDs []int64
	ch, err := job.pgNotifier.Publish(StagingFileProcessPGChannel, messages)
	if err != nil {
		panic(err)
	}

	responses := <-ch
	logger.Infof("[WH]: Received responses from PgNotifier")
	for _, resp := range responses {
		// TODO: make it aborted
		if resp.Status == "aborted" {
			logger.Errorf("Error in genrating load files: %v", resp.Error)
			continue
		}
		var payload map[string]interface{}
		err = json.Unmarshal(resp.Payload, &payload)
		if err != nil {
			panic(err)
		}
		respIDs, ok := payload["LoadFileIDs"].([]interface{})
		if !ok {
			logger.Errorf("No LoadFIleIDS returned by wh worker")
			continue
		}
		ids := make([]int64, len(respIDs))
		for i := range respIDs {
			ids[i] = int64(respIDs[i].(float64))
		}
		loadFileIDs = append(loadFileIDs, ids...)
	}

	timer.End()
	if len(loadFileIDs) == 0 {
		err = fmt.Errorf(responses[0].Error)
		return loadFileIDs, err
	}
	sort.Slice(loadFileIDs, func(i, j int) bool { return loadFileIDs[i] < loadFileIDs[j] })
	return loadFileIDs, nil
}

func (job *UploadJobT) updateTableEventsCount(tableName string) (err error) {
	subQuery := fmt.Sprintf(`SELECT sum(total_events) as total from %[1]s right join (
		SELECT  staging_file_id, MAX(id) AS id FROM wh_load_files
		WHERE ( source_id='%[2]s'
			AND destination_id='%[3]s'
			AND table_name='%[4]s'
			AND id >= %[5]v
			AND id <= %[6]v)
		GROUP BY staging_file_id ) uniqueStagingFiles
		ON  wh_load_files.id = uniqueStagingFiles.id `,
		warehouseutils.WarehouseLoadFilesTable,
		job.warehouse.Source.ID,
		job.warehouse.Destination.ID,
		tableName,
		job.upload.StartLoadFileID,
		job.upload.EndLoadFileID,
		warehouseutils.WarehouseTableUploadsTable)

	sqlStatement := fmt.Sprintf(`update %[1]s set total_events = subquery.total FROM (%[2]s) AS subquery WHERE table_name = '%[3]s' AND wh_upload_id = %[4]d`,
		warehouseutils.WarehouseTableUploadsTable,
		subQuery,
		tableName,
		job.upload.ID)
	_, err = job.dbHandle.Exec(sqlStatement)
	return
}

func (job *UploadJobT) areIdentityTablesLoadFilesGenerated() (generated bool, err error) {
	var mergeRulesLocation sql.NullString
	sqlStatement := fmt.Sprintf(`SELECT location FROM %s WHERE wh_upload_id=%d AND table_name='%s'`, warehouseutils.WarehouseTableUploadsTable, job.upload.ID, warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.IdentityMergeRulesTable))
	err = job.dbHandle.QueryRow(sqlStatement).Scan(&mergeRulesLocation)
	if err != nil {
		return
	}
	if !mergeRulesLocation.Valid {
		generated = false
		return
	}

	var mappingsLocation sql.NullString
	sqlStatement = fmt.Sprintf(`SELECT location FROM %s WHERE wh_upload_id=%d AND table_name='%s'`, warehouseutils.WarehouseTableUploadsTable, job.upload.ID, warehouseutils.ToProviderCase(job.warehouse.Type, warehouseutils.IdentityMergeRulesTable))
	err = job.dbHandle.QueryRow(sqlStatement).Scan(&mappingsLocation)
	if err != nil {
		return
	}
	if !mappingsLocation.Valid {
		generated = false
		return
	}
	generated = true
	return
}

func (job *UploadJobT) GetLoadFileLocations(tableName string) (locations []string, err error) {
	sqlStatement := fmt.Sprintf(`SELECT location from %[1]s right join (
		SELECT  staging_file_id, MAX(id) AS id FROM wh_load_files
		WHERE ( source_id='%[2]s'
			AND destination_id='%[3]s'
			AND table_name='%[4]s'
			AND id >= %[5]v
			AND id <= %[6]v)
		GROUP BY staging_file_id ) uniqueStagingFiles
		ON  wh_load_files.id = uniqueStagingFiles.id `,
		warehouseutils.WarehouseLoadFilesTable,
		job.warehouse.Source.ID,
		job.warehouse.Destination.ID,
		tableName,
		job.upload.StartLoadFileID,
		job.upload.EndLoadFileID,
	)
	rows, err := dbHandle.Query(sqlStatement)
	if err != nil {
		panic(err)
	}
	defer rows.Close()

	for rows.Next() {
		var location string
		err := rows.Scan(&location)
		if err != nil {
			panic(err)
		}
		locations = append(locations, location)
	}
	return
}

func (job *UploadJobT) GetSampleLoadFileLocation(tableName string) (location string, err error) {
	sqlStatement := fmt.Sprintf(`SELECT location FROM %[1]s RIGHT JOIN (
		SELECT  staging_file_id, MAX(id) AS id FROM %[1]s
		WHERE ( source_id='%[2]s'
			AND destination_id='%[3]s'
			AND table_name='%[4]s'
			AND id >= %[5]v
			AND id <= %[6]v)
		GROUP BY staging_file_id ) uniqueStagingFiles
		ON  wh_load_files.id = uniqueStagingFiles.id `,
		warehouseutils.WarehouseLoadFilesTable,
		job.warehouse.Source.ID,
		job.warehouse.Destination.ID,
		tableName,
		job.upload.StartLoadFileID,
		job.upload.EndLoadFileID,
	)
	err = dbHandle.QueryRow(sqlStatement).Scan(&location)
	if err != nil && err != sql.ErrNoRows {
		panic(err)
	}
	return
}

func (job *UploadJobT) GetSchemaInWarehouse() (schema warehouseutils.SchemaT) {
	if job.schemaHandle == nil {
		return
	}
	return job.schemaHandle.schemaInWarehouse
}

func (job *UploadJobT) GetTableSchemaAfterUpload(tableName string) warehouseutils.TableSchemaT {
	return job.schemaHandle.schemaAfterUpload[tableName]
}

func (job *UploadJobT) GetTableSchemaInUpload(tableName string) warehouseutils.TableSchemaT {
	return job.schemaHandle.uploadSchema[tableName]
}

func (job *UploadJobT) GetSingleLoadFileLocation(tableName string) (string, error) {
	sqlStatement := fmt.Sprintf(`SELECT location FROM %s WHERE wh_upload_id=%d AND table_name='%s'`, warehouseutils.WarehouseTableUploadsTable, job.upload.ID, tableName)
	logger.Infof("SF: Fetching load file location for %s: %s", tableName, sqlStatement)
	var location string
	err := job.dbHandle.QueryRow(sqlStatement).Scan(&location)
	return location, err
}
