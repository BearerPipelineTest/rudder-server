package router

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	jsoniter "github.com/json-iterator/go"

	"github.com/tidwall/sjson"

	"github.com/gofrs/uuid"
	"github.com/golang/mock/gomock"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rudderlabs/rudder-server/admin"
	"github.com/rudderlabs/rudder-server/config"
	backendconfig "github.com/rudderlabs/rudder-server/config/backend-config"
	"github.com/rudderlabs/rudder-server/jobsdb"
	mocksBackendConfig "github.com/rudderlabs/rudder-server/mocks/config/backend-config"
	mocksJobsDB "github.com/rudderlabs/rudder-server/mocks/jobsdb"
	mocksRouter "github.com/rudderlabs/rudder-server/mocks/router"
	mocksTransformer "github.com/rudderlabs/rudder-server/mocks/router/transformer"
	mocksMultitenant "github.com/rudderlabs/rudder-server/mocks/services/multitenant"
	"github.com/rudderlabs/rudder-server/router/types"
	routerUtils "github.com/rudderlabs/rudder-server/router/utils"
	"github.com/rudderlabs/rudder-server/services/rsources"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/services/transientsource"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/pubsub"
	testutils "github.com/rudderlabs/rudder-server/utils/tests"
	utilTypes "github.com/rudderlabs/rudder-server/utils/types"
)

const (
	writeKeyEnabled = "enabled-write-key"
	// writeKeyDisabled          = "disabled-write-key"
	// writeKeyInvalid           = "invalid-write-key"
	// writeKeyEmpty             = ""
	sourceIDEnabled = "enabled-source"
	// sourceIDDisabled          = "disabled-source"
	// testRemoteAddressWithPort = "test.com:80"
	// testRemoteAddress         = "test.com"
	gaDestinationDefinitionID = "gaid1"
	gaDestinationID           = "did1"
)

var (
	testTimeout             = 10 * time.Second
	customVal               = map[string]string{"GA": "GA"}
	workspaceID             = uuid.Must(uuid.NewV4()).String()
	gaDestinationDefinition = backendconfig.DestinationDefinitionT{
		ID:          gaDestinationDefinitionID,
		Name:        "GA",
		DisplayName: "Google Analytics",
	}
	collectMetricsErrorMap = map[string]int{
		"Error Response 1":  1,
		"Error Response 2":  2,
		"Error Response 3":  3,
		"Error Response 4":  4,
		"Error Response 5":  1,
		"Error Response 6":  2,
		"Error Response 7":  3,
		"Error Response 8":  4,
		"Error Response 9":  1,
		"Error Response 10": 2,
		"Error Response 11": 3,
		"Error Response 12": 4,
		"Error Response 13": 1,
		"Error Response 14": 2,
		"Error Response 15": 3,
		"Error Response 16": 4,
	}
	// This configuration is assumed by all router tests and, is returned on Subscribe of mocked backend config
	sampleBackendConfig = backendconfig.ConfigT{
		Sources: []backendconfig.SourceT{
			{
				WorkspaceID: workspaceID,
				ID:          sourceIDEnabled,
				WriteKey:    writeKeyEnabled,
				Enabled:     true,
				Destinations: []backendconfig.DestinationT{
					{
						ID:                    gaDestinationID,
						Name:                  "ga dest",
						DestinationDefinition: gaDestinationDefinition,
						Enabled:               true,
						IsProcessorEnabled:    true,
					},
				},
			},
		},
	}
)

type reportingNOOP struct{}

func (*reportingNOOP) WaitForSetup(_ context.Context, _ string)          {}
func (*reportingNOOP) Report(_ []*utilTypes.PUReportedMetric, _ *sql.Tx) {}

type testContext struct {
	asyncHelper     testutils.AsyncTestHelper
	dbReadBatchSize int

	mockCtrl          *gomock.Controller
	mockRouterJobsDB  *mocksJobsDB.MockMultiTenantJobsDB
	mockProcErrorsDB  *mocksJobsDB.MockJobsDB
	mockBackendConfig *mocksBackendConfig.MockBackendConfig
}

// Initialize mocks and common expectations
func (c *testContext) Setup() {
	c.asyncHelper.Setup()
	c.mockCtrl = gomock.NewController(GinkgoT())
	c.mockRouterJobsDB = mocksJobsDB.NewMockMultiTenantJobsDB(c.mockCtrl)
	c.mockProcErrorsDB = mocksJobsDB.NewMockJobsDB(c.mockCtrl)
	c.mockBackendConfig = mocksBackendConfig.NewMockBackendConfig(c.mockCtrl)

	tFunc := c.asyncHelper.ExpectAndNotifyCallbackWithName("backend_config")

	// During Setup, router subscribes to backend config
	c.mockBackendConfig.EXPECT().Subscribe(gomock.Any(), backendconfig.TopicBackendConfig).
		DoAndReturn(func(ctx context.Context, topic backendconfig.Topic) pubsub.DataChannel {
			tFunc()
			ch := make(chan pubsub.DataEvent, 1)
			ch <- pubsub.DataEvent{Data: sampleBackendConfig, Topic: string(topic)}
			// on Subscribe, emulate a backend configuration event
			go func() {
				<-ctx.Done()
				close(ch)
			}()
			return ch
		})
	c.dbReadBatchSize = 10000
}

func (c *testContext) Finish() {
	c.asyncHelper.WaitWithTimeout(testTimeout)
	c.mockCtrl.Finish()
}

func initRouter() {
	config.Load()
	admin.Init()
	logger.Init()
	Init()
	InitRouterAdmin()
}

var _ = Describe("Router", func() {
	initRouter()

	var c *testContext

	BeforeEach(func() {
		routerUtils.JobRetention = time.Duration(175200) * time.Hour // 20 Years(20*365*24)
		c = &testContext{}
		c.Setup()
		// setup static requirements of dependencies
		stats.Setup()
	})

	AfterEach(func() {
		c.Finish()
	})

	Context("Initialization", func() {
		It("should initialize and recover after crash", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())
		})
	})

	Context("normal operation - ga", func() {
		BeforeEach(func() {
			maxStatusUpdateWait = 2 * time.Second
		})

		It("should send failed, unprocessed jobs to ga destination", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()

			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)
			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters:  []byte(parameters),
					WorkspaceId: workspaceID,
				},
			}

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters:  []byte(parameters),
					WorkspaceId: workspaceID,
				},
			}

			allJobs := append(toRetryJobsList, unprocessedJobsList...)
			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount

			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callGetAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount,
				jobsdb.GetQueryParamsT{CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: workspaceCount[workspaceID]}, 10).Times(1).Return(allJobs, nil).After(callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil).After(callGetAllJobs)

			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(2).Return(
				&routerUtils.SendPostResponse{StatusCode: 200, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), true, false).AnyTimes()
			done := make(chan struct{})

			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, _ interface{}, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Succeeded.State, "200", `{"content-type":"","response": "","firstAttemptedAt":"2021-06-28T15:57:30.742+05:30"}`, 2)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Succeeded.State, "200", `{"content-type":"","response": "","firstAttemptedAt":"2021-06-28T15:57:30.742+05:30"}`, 1)
				})

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(2))
			<-done
		})

		It("should abort unprocessed jobs to ga destination because of bad payload", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)

			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()

			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())

			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			router.netHandle = mockNetHandle

			gaPayload := `{}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters:  []byte(parameters),
					WorkspaceId: workspaceID,
				},
			}

			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList)
			workspaceCountOut := workspaceCount

			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callGetAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount, jobsdb.GetQueryParamsT{
				CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: workspaceCount[workspaceID],
			}, 10).Times(1).Return(unprocessedJobsList, nil).After(callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(unprocessedJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 0)
				}).After(callGetAllJobs)

			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(1).Return(&routerUtils.SendPostResponse{StatusCode: 400, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			c.mockProcErrorsDB.EXPECT().Store(gomock.Any(), gomock.Any()).Times(1).
				Do(func(ctx context.Context, jobList []*jobsdb.JobT) {
					job := jobList[0]
					var parameters map[string]interface{}
					err := json.Unmarshal(job.Parameters, &parameters)
					if err != nil {
						panic(err)
					}

					Expect(parameters["stage"]).To(Equal("router"))
					Expect(job.JobID).To(Equal(unprocessedJobsList[0].JobID))
					Expect(job.CustomVal).To(Equal(unprocessedJobsList[0].CustomVal))
					Expect(job.UserID).To(Equal(unprocessedJobsList[0].UserID))
				})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), false, true).AnyTimes()
			done := make(chan struct{})
			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, _ interface{}, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(unprocessedJobsList[0], statuses[0], jobsdb.Aborted.State, "400", `{"content-type":"","response":"","firstAttemptedAt":"2021-06-28T15:57:30.742+05:30"}`, 1)
				})

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(1))
			<-done
		})

		It("aborts events that are older than a configurable duration", func() {
			routerUtils.JobRetention = time.Duration(24) * time.Hour
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()

			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			router.netHandle = mockNetHandle
			router.MultitenantI = mockMultitenantHandle

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters:  []byte(parameters),
					WorkspaceId: workspaceID,
				},
			}

			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList)
			workspaceCountOut := workspaceCount

			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount, jobsdb.GetQueryParamsT{
				CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: workspaceCount[workspaceID],
			}, 10).Times(1).Return(unprocessedJobsList, nil).After(callGetRouterPickupJobs)

			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), gomock.Any(),
				gomock.Any()).Times(1)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1)

			c.mockProcErrorsDB.EXPECT().Store(gomock.Any(), gomock.Any()).Times(1).
				Do(func(ctx context.Context, jobList []*jobsdb.JobT) {
					job := jobList[0]
					var parameters map[string]interface{}
					err := json.Unmarshal(job.Parameters, &parameters)
					if err != nil {
						panic(err)
					}

					Expect(job.JobID).To(Equal(unprocessedJobsList[0].JobID))
					Expect(job.CustomVal).To(Equal(unprocessedJobsList[0].CustomVal))
					Expect(job.UserID).To(Equal(unprocessedJobsList[0].UserID))
				})

			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
			}).Return(nil).Times(1)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, tx jobsdb.UpdateSafeTx, drainList []*jobsdb.JobStatusT, _, _ interface{}) {
					Expect(drainList).To(HaveLen(1))
					assertJobStatus(unprocessedJobsList[0], drainList[0], jobsdb.Aborted.State, "410", `{"reason": "job expired"}`, 0)
				})

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(0))
		})

		It("can fail jobs if time is more than router timeout", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			mockTransformer := mocksTransformer.NewMockTransformer(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())
			router.transformer = mockTransformer
			router.noOfWorkers = 1
			router.noOfJobsToBatchInAWorker = 5
			router.routerTimeout = time.Duration(0)

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)

			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters: []byte(parameters),
				},
			}

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2011,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2012,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u3",
					JobID:        2013,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
			}

			allJobs := append(toRetryJobsList, unprocessedJobsList...)
			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount
			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount,
				jobsdb.GetQueryParamsT{CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: len(allJobs)}, 10).Times(1).Return(allJobs, nil).After(
				callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[1], statuses[2], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[2], statuses[3], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[3], statuses[4], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil).After(callAllJobs)
			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(0)
			done := make(chan struct{})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1)

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(5))
			<-done
		})
	})

	Context("Router Batching", func() {
		BeforeEach(func() {
			maxStatusUpdateWait = 2 * time.Second
		})

		It("can batch jobs together", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			mockTransformer := mocksTransformer.NewMockTransformer(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())

			router.transformer = mockTransformer

			router.enableBatching = true
			router.noOfJobsToBatchInAWorker = 3
			router.noOfWorkers = 1
			router.routerTimeout = time.Duration(math.MaxInt64)

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)

			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters: []byte(parameters),
				},
			}

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u3",
					JobID:        2011,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 27, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 27, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
			}

			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount
			jobsList := append(toRetryJobsList, unprocessedJobsList...)
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()

			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount, jobsdb.GetQueryParamsT{
				CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: len(jobsList),
			}, 10).Times(1).Return(jobsList, nil).After(callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[1], statuses[2], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil)

			mockTransformer.EXPECT().Transform("BATCH", gomock.Any()).After(callAllJobs).Times(1).
				DoAndReturn(
					func(_ string, transformMessage *types.TransformMessageT) []types.DestinationJobT {
						assertRouterJobs(&transformMessage.Data[0], toRetryJobsList[0])
						assertRouterJobs(&transformMessage.Data[1], unprocessedJobsList[0])
						assertRouterJobs(&transformMessage.Data[2], unprocessedJobsList[1])
						return []types.DestinationJobT{
							{
								Message: []byte(`{"message": "some transformed message"}`),
								JobMetadataArray: []types.JobMetadataT{
									{
										UserID: "u1",
										JobID:  2009,
										JobT:   toRetryJobsList[0],
									},
									{
										UserID: "u2",
										JobID:  2010,
										JobT:   unprocessedJobsList[0],
									},
									{
										UserID: "u3",
										JobID:  2011,
										JobT:   unprocessedJobsList[1],
									},
								},
								Batched:    true,
								Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
								StatusCode: 200,
							},
						}
					})

			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(1).Return(&routerUtils.SendPostResponse{StatusCode: 200, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			done := make(chan struct{})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), true, false).AnyTimes()
			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, _ interface{}, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertTransformJobStatuses(toRetryJobsList[0], statuses[0], jobsdb.Succeeded.State, "200", 1)
					assertTransformJobStatuses(unprocessedJobsList[0], statuses[1], jobsdb.Succeeded.State, "200", 1)
					assertTransformJobStatuses(unprocessedJobsList[1], statuses[2], jobsdb.Succeeded.State, "200", 1)
				})

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(3))
			<-done
		})

		It("aborts jobs if batching fails for few of the jobs", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			mockTransformer := mocksTransformer.NewMockTransformer(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())

			// we have a job that has failed once(toRetryJobsList), it should abort when picked up next
			// Because we only allow one failure per job with this
			router.transformer = mockTransformer
			router.noOfJobsToBatchInAWorker = 3
			router.maxFailedCountForJob = 5
			router.enableBatching = true

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "processor"}`, gaDestinationID)

			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters: []byte(parameters),
				},
			}
			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2011,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 27, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 27, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
			}
			allJobs := append(toRetryJobsList, unprocessedJobsList...)
			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount
			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1).Return(workspaceCountOut, map[string]float64{})

			payloadLimit := router.payloadLimit
			callAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount,
				jobsdb.GetQueryParamsT{CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: len(allJobs)}, 10).Return(toRetryJobsList, nil).Times(
				1).Return(allJobs, nil).After(callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[1], statuses[2], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil).After(callAllJobs)

			mockTransformer.EXPECT().Transform("BATCH", gomock.Any()).After(callAllJobs).Times(1).DoAndReturn(
				func(_ string, transformMessage *types.TransformMessageT) []types.DestinationJobT {
					assertRouterJobs(&transformMessage.Data[0], toRetryJobsList[0])
					assertRouterJobs(&transformMessage.Data[1], unprocessedJobsList[0])
					assertRouterJobs(&transformMessage.Data[2], unprocessedJobsList[1])

					return []types.DestinationJobT{
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID:           "u1",
									JobID:            2009,
									JobT:             toRetryJobsList[0],
									FirstAttemptedAt: "2021-06-28T15:57:30.742+05:30",
									AttemptNum:       1,
								},
								{
									UserID:           "u1",
									JobID:            2010,
									JobT:             unprocessedJobsList[0],
									FirstAttemptedAt: "2021-06-28T15:57:30.742+05:30",
								},
							},
							Batched:    true,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 500,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u1",
									JobID:  2011,
									JobT:   unprocessedJobsList[1],
								},
							},
							Batched:    true,
							Error:      ``,
							StatusCode: 200,
						},
					}
				})

			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(0).Return(&routerUtils.SendPostResponse{StatusCode: 200, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			done := make(chan struct{})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, _ interface{}, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertTransformJobStatuses(toRetryJobsList[0], statuses[0], jobsdb.Failed.State, "500", 1)
					assertTransformJobStatuses(unprocessedJobsList[0], statuses[1], jobsdb.Waiting.State, "", 0)
					assertTransformJobStatuses(unprocessedJobsList[1], statuses[2], jobsdb.Waiting.State, "", 0)
				})

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(3))
			<-done
		})
	})

	Context("Router Transform", func() {
		BeforeEach(func() {
			maxStatusUpdateWait = 2 * time.Second
			jobsBatchTimeout = 10 * time.Second
		})
		/*
			Router transform
				Job1 u1
				Job2 u1
				Job3 u2
				Job4 u2
				Job5 u3
			{[1]: T200 [2]: T500, [3]: T500, [4]: T200, [5]: T200}

			[1] should be sent
			[2] shouldn't be sent
			[3] shouldn't be sent
			[4] should be dropped
			[5] should be sent
		*/
		It("can transform jobs at router", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			mockTransformer := mocksTransformer.NewMockTransformer(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())
			router.transformer = mockTransformer
			router.noOfWorkers = 1
			router.noOfJobsToBatchInAWorker = 5
			router.routerTimeout = time.Duration(math.MaxInt64)

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "router"}`, gaDestinationID)

			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters: []byte(parameters),
				},
			}

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2011,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2012,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u3",
					JobID:        2013,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
			}

			allJobs := append(toRetryJobsList, unprocessedJobsList...)
			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount
			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount,
				jobsdb.GetQueryParamsT{CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: len(allJobs)}, 10).Times(1).Return(allJobs, nil).After(
				callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[1], statuses[2], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[2], statuses[3], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[3], statuses[4], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil).After(callAllJobs)

			mockTransformer.EXPECT().Transform("ROUTER_TRANSFORM", gomock.Any()).After(callAllJobs).Times(1).DoAndReturn(
				func(_ string, transformMessage *types.TransformMessageT) []types.DestinationJobT {
					assertRouterJobs(&transformMessage.Data[0], toRetryJobsList[0])
					assertRouterJobs(&transformMessage.Data[1], unprocessedJobsList[0])
					assertRouterJobs(&transformMessage.Data[2], unprocessedJobsList[1])
					assertRouterJobs(&transformMessage.Data[3], unprocessedJobsList[2])
					assertRouterJobs(&transformMessage.Data[4], unprocessedJobsList[3])

					return []types.DestinationJobT{
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID:     "u1",
									JobID:      2009,
									AttemptNum: 1,
									JobT:       toRetryJobsList[0],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 200,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u1",
									JobID:  2010,
									JobT:   unprocessedJobsList[0],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 500,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u2",
									JobID:  2011,
									JobT:   unprocessedJobsList[1],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 500,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u2",
									JobID:  2012,
									JobT:   unprocessedJobsList[2],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 200,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u3",
									JobID:  2013,
									JobT:   unprocessedJobsList[3],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 200,
						},
					}
				})

			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(2).Return(&routerUtils.SendPostResponse{StatusCode: 200, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			done := make(chan struct{})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1)

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(5))
			<-done
		})

		/*
				Job1 u1
				Job2 u1
				Job3 u1
			{[1]: T500, [T2]: T200, [3]: T200}

				[1] shouldn't be sent
				[2] should be dropped
				[3] should be dropped
		*/
		It("marks all jobs of a user failed if a preceding job fails due to transformation failure", func() {
			mockMultitenantHandle := mocksMultitenant.NewMockMultiTenantI(c.mockCtrl)
			mockNetHandle := mocksRouter.NewMockNetHandleI(c.mockCtrl)
			router := &HandleT{
				Reporting:    &reportingNOOP{},
				MultitenantI: mockMultitenantHandle,
				netHandle:    mockNetHandle,
			}
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			c.mockBackendConfig.EXPECT().AccessToken().AnyTimes()
			router.Setup(c.mockBackendConfig, c.mockRouterJobsDB, c.mockProcErrorsDB, gaDestinationDefinition, transientsource.NewEmptyService(), rsources.NewNoOpService())
			mockTransformer := mocksTransformer.NewMockTransformer(c.mockCtrl)
			router.transformer = mockTransformer

			router.noOfJobsToBatchInAWorker = 3
			router.noOfWorkers = 1

			gaPayload := `{"body": {"XML": {}, "FORM": {}, "JSON": {}}, "type": "REST", "files": {}, "method": "POST", "params": {"t": "event", "v": "1", "an": "RudderAndroidClient", "av": "1.0", "ds": "android-sdk", "ea": "Demo Track", "ec": "Demo Category", "el": "Demo Label", "ni": 0, "qt": 59268380964, "ul": "en-US", "cid": "anon_id", "tid": "UA-185645846-1", "uip": "[::1]", "aiid": "com.rudderlabs.android.sdk"}, "userId": "anon_id", "headers": {}, "version": "1", "endpoint": "https://www.google-analytics.com/collect"}`
			parameters := fmt.Sprintf(`{"source_id": "1fMCVYZboDlYlauh4GFsEo2JU77", "destination_id": "%s", "message_id": "2f548e6d-60f6-44af-a1f4-62b3272445c3", "received_at": "2021-06-28T10:04:48.527+05:30", "transform_at": "router"}`, gaDestinationID)

			toRetryJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2009,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum:    1,
						ErrorResponse: []byte(`{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`),
					},
					Parameters: []byte(parameters),
				},
			}

			unprocessedJobsList := []*jobsdb.JobT{
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u1",
					JobID:        2010,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
				{
					UUID:         uuid.Must(uuid.NewV4()),
					UserID:       "u2",
					JobID:        2011,
					CreatedAt:    time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					ExpireAt:     time.Date(2020, 0o4, 28, 13, 26, 0o0, 0o0, time.UTC),
					CustomVal:    customVal["GA"],
					EventPayload: []byte(gaPayload),
					LastJobStatus: jobsdb.JobStatusT{
						AttemptNum: 0,
					},
					Parameters: []byte(parameters),
				},
			}

			allJobs := append(toRetryJobsList, unprocessedJobsList...)
			workspaceCount := map[string]int{}
			workspaceCount[workspaceID] = len(unprocessedJobsList) + len(toRetryJobsList)
			workspaceCountOut := workspaceCount
			callGetRouterPickupJobs := mockMultitenantHandle.EXPECT().GetRouterPickupJobs(customVal["GA"], gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(workspaceCountOut, map[string]float64{}).Times(1)

			payloadLimit := router.payloadLimit
			callAllJobs := c.mockRouterJobsDB.EXPECT().GetAllJobs(gomock.Any(), workspaceCount,
				jobsdb.GetQueryParamsT{CustomValFilters: []string{customVal["GA"]}, PayloadSizeLimit: payloadLimit, JobsLimit: len(allJobs)}, 10).Times(1).Return(allJobs, nil).After(callGetRouterPickupJobs)

			c.mockRouterJobsDB.EXPECT().UpdateJobStatus(gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1).
				Do(func(ctx context.Context, statuses []*jobsdb.JobStatusT, _, _ interface{}) {
					assertJobStatus(toRetryJobsList[0], statuses[0], jobsdb.Executing.State, "", `{}`, 1)
					assertJobStatus(unprocessedJobsList[0], statuses[1], jobsdb.Executing.State, "", `{}`, 0)
					assertJobStatus(unprocessedJobsList[1], statuses[2], jobsdb.Executing.State, "", `{}`, 0)
				}).Return(nil).After(callAllJobs)

			mockTransformer.EXPECT().Transform("ROUTER_TRANSFORM", gomock.Any()).After(callAllJobs).Times(1).DoAndReturn(
				func(_ string, transformMessage *types.TransformMessageT) []types.DestinationJobT {
					assertRouterJobs(&transformMessage.Data[0], toRetryJobsList[0])
					assertRouterJobs(&transformMessage.Data[1], unprocessedJobsList[0])
					assertRouterJobs(&transformMessage.Data[2], unprocessedJobsList[1])

					return []types.DestinationJobT{
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID:     "u1",
									JobID:      2009,
									AttemptNum: 1,
									JobT:       toRetryJobsList[0],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 500,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u1",
									JobID:  2010,
									JobT:   unprocessedJobsList[0],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 200,
						},
						{
							Message: []byte(`{"message": "some transformed message"}`),
							JobMetadataArray: []types.JobMetadataT{
								{
									UserID: "u1",
									JobID:  2010,
									JobT:   unprocessedJobsList[0],
								},
							},
							Batched:    false,
							Error:      `{"firstAttemptedAt": "2021-06-28T15:57:30.742+05:30"}`,
							StatusCode: 200,
						},
					}
				})
			mockNetHandle.EXPECT().SendPost(gomock.Any(), gomock.Any()).Times(0).Return(&routerUtils.SendPostResponse{StatusCode: 200, ResponseBody: []byte("")})
			mockMultitenantHandle.EXPECT().UpdateWorkspaceLatencyMap(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			done := make(chan struct{})
			mockMultitenantHandle.EXPECT().CalculateSuccessFailureCounts(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()

			c.mockRouterJobsDB.EXPECT().WithUpdateSafeTx(gomock.Any()).Times(1).Do(func(f func(tx jobsdb.UpdateSafeTx) error) {
				_ = f(jobsdb.EmptyUpdateSafeTx())
				close(done)
			}).Return(nil)
			c.mockRouterJobsDB.EXPECT().UpdateJobStatusInTx(gomock.Any(), gomock.Any(), gomock.Any(), []string{customVal["GA"]}, nil).Times(1)

			<-router.backendConfigInitialized
			count := router.readAndProcess()
			Expect(count).To(Equal(3))
			<-done
		})
	})
})

func assertRouterJobs(routerJob *types.RouterJobT, job *jobsdb.JobT) {
	Expect(routerJob.JobMetadata.JobID).To(Equal(job.JobID))
	Expect(routerJob.JobMetadata.UserID).To(Equal(job.UserID))
}

func assertJobStatus(job *jobsdb.JobT, status *jobsdb.JobStatusT, expectedState, errorCode, errorResponse string, attemptNum int) {
	Expect(status.JobID).To(Equal(job.JobID))
	Expect(status.JobState).To(Equal(expectedState))
	Expect(status.ErrorCode).To(Equal(errorCode))
	if attemptNum > 1 {
		Expect(status.ErrorResponse).To(MatchJSON(errorResponse))
	}
	Expect(status.RetryTime).To(BeTemporally("~", time.Now(), 10*time.Second))
	Expect(status.ExecTime).To(BeTemporally("~", time.Now(), 10*time.Second))
	Expect(status.AttemptNum).To(Equal(attemptNum))
}

func assertTransformJobStatuses(job *jobsdb.JobT, status *jobsdb.JobStatusT, expectedState, errorCode string, attemptNum int) {
	Expect(status.JobID).To(Equal(job.JobID))
	Expect(status.JobState).To(Equal(expectedState))
	Expect(status.ErrorCode).To(Equal(errorCode))
	Expect(status.RetryTime).To(BeTemporally("~", time.Now(), 10*time.Second))
	Expect(status.ExecTime).To(BeTemporally("~", time.Now(), 10*time.Second))
	Expect(status.AttemptNum).To(Equal(attemptNum))
}

func Benchmark_SJSON_SET(b *testing.B) {
	var stringValue string
	var err error
	for i := 0; i < b.N; i++ {
		for k, v := range collectMetricsErrorMap {
			stringValue, err = sjson.Set(stringValue, k, v)
			if err != nil {
				stringValue = ""
			}
		}
	}
}

func Benchmark_JSON_MARSHAL(b *testing.B) {
	for i := 0; i < b.N; i++ {
		val, _ := json.Marshal(collectMetricsErrorMap)
		_ = string(val)
	}
}

func Benchmark_FASTJSON_MARSHAL(b *testing.B) {
	jsonfast := jsoniter.ConfigCompatibleWithStandardLibrary

	for i := 0; i < b.N; i++ {
		val, _ := jsonfast.Marshal(collectMetricsErrorMap)
		_ = string(val)
	}
}
