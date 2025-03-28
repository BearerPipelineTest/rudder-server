//go:generate mockgen --build_flags=--mod=mod -destination=../../../mocks/gateway/webhook/mock_webhook.go -package mock_webhook github.com/rudderlabs/rudder-server/gateway/webhook GatewayI

package webhook

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/rudderlabs/rudder-server/config"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils/misc"
	"golang.org/x/sync/errgroup"
)

type GatewayI interface {
	IncrementRecvCount(count uint64)
	IncrementAckCount(count uint64)
	UpdateSourceStats(writeKeyStats map[string]int, bucket string, sourceTagMap map[string]string)
	TrackRequestMetrics(errorMessage string)
	ProcessWebRequest(writer *http.ResponseWriter, req *http.Request, reqType string, requestPayload []byte, writeKey string) string
	GetWebhookSourceDefName(writeKey string) (name string, ok bool)
}

type WebHookI interface {
	RequestHandler(w http.ResponseWriter, r *http.Request)
	Register(name string)
}

func newWebhookStats() *webhookStatsT {
	wStats := webhookStatsT{}
	wStats.sentStat = stats.DefaultStats.NewStat("webhook.transformer_sent", stats.CountType)
	wStats.receivedStat = stats.DefaultStats.NewStat("webhook.transformer_received", stats.CountType)
	wStats.failedStat = stats.DefaultStats.NewStat("webhook.transformer_failed", stats.CountType)
	wStats.transformTimerStat = stats.DefaultStats.NewStat("webhook.transformation_time", stats.TimerType)
	wStats.sourceStats = make(map[string]*webhookSourceStatT)
	return &wStats
}

func Setup(gwHandle GatewayI, opts ...batchTransformerOption) *HandleT {
	webhook := &HandleT{gwHandle: gwHandle}
	webhook.requestQ = make(map[string](chan *webhookT))
	webhook.batchRequestQ = make(chan *batchWebhookT)
	webhook.netClient = retryablehttp.NewClient()
	webhook.netClient.HTTPClient.Timeout = config.GetDuration("HttpClient.webhook.timeout", 30, time.Second)
	webhook.netClient.Logger = nil // to avoid debug logs
	webhook.netClient.RetryWaitMin = webhookRetryWaitMin
	webhook.netClient.RetryWaitMax = webhookRetryWaitMax
	webhook.netClient.RetryMax = webhookRetryMax

	ctx, cancel := context.WithCancel(context.Background())
	webhook.backgroundCancel = cancel

	g, _ := errgroup.WithContext(ctx)
	for i := 0; i < maxTransformerProcess; i++ {
		g.Go(misc.WithBugsnag(func() error {
			bt := batchWebhookTransformerT{
				webhook:              webhook,
				stats:                newWebhookStats(),
				sourceTransformerURL: sourceTransformerURL,
			}
			for _, opt := range opts {
				opt(&bt)
			}
			bt.batchTransformLoop()
			return nil
		}))
	}
	g.Go(misc.WithBugsnag(func() error {
		webhook.printStats(ctx)
		return nil
	}))

	webhook.backgroundWait = g.Wait
	return webhook
}
