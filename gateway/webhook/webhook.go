package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-retryablehttp"
	"github.com/rudderlabs/rudder-server/gateway/response"
	"github.com/rudderlabs/rudder-server/services/stats"
	"github.com/rudderlabs/rudder-server/utils/logger"
	"github.com/rudderlabs/rudder-server/utils/misc"
)

var (
	sourceTransformerURL       string
	webhookBatchTimeout        time.Duration
	maxTransformerProcess      int
	maxWebhookBatchSize        int
	webhookRetryMax            int
	webhookRetryWaitMax        time.Duration
	webhookRetryWaitMin        time.Duration
	pkgLogger                  logger.LoggerI
	sourceListForParsingParams []string
)

func Init() {
	loadConfig()
	pkgLogger = logger.NewLogger().Child("gateway").Child("webhook")
}

type webhookT struct {
	request    *http.Request
	writer     http.ResponseWriter
	done       chan<- transformerResponse
	sourceType string
	writeKey   string
}

type batchWebhookT struct {
	batchRequest []*webhookT
	sourceType   string
}

type HandleT struct {
	requestQ      map[string]chan *webhookT
	batchRequestQ chan *batchWebhookT
	netClient     *retryablehttp.Client
	gwHandle      GatewayI
	ackCount      uint64
	recvCount     uint64

	batchRequestsWg  sync.WaitGroup
	backgroundWait   func() error
	backgroundCancel context.CancelFunc
}

type webhookSourceStatT struct {
	id              string
	numEvents       stats.RudderStats
	numOutputEvents stats.RudderStats
	sourceTransform stats.RudderStats
}

type webhookStatsT struct {
	sentStat           stats.RudderStats
	receivedStat       stats.RudderStats
	failedStat         stats.RudderStats
	transformTimerStat stats.RudderStats
	sourceStats        map[string]*webhookSourceStatT
}

type batchWebhookTransformerT struct {
	webhook              *HandleT
	stats                *webhookStatsT
	sourceTransformerURL string
}

type batchTransformerOption func(bt *batchWebhookTransformerT)

func parseWriteKey(req *http.Request) (writeKey string, found bool) {
	queryParams := req.URL.Query()
	writeKeys, found := queryParams["writeKey"]
	if found && writeKeys[0] != "" {
		writeKey = writeKeys[0]
	} else {
		writeKey, _, found = req.BasicAuth()
	}
	return
}

func (webhook *HandleT) failRequest(w http.ResponseWriter, r *http.Request, reason string, code int, stat string) {
	writeKeyFailStats := make(map[string]int)
	misc.IncrementMapByKey(writeKeyFailStats, stat, 1)
	webhook.gwHandle.UpdateSourceStats(writeKeyFailStats, "gateway.write_key_failed_requests", map[string]string{stat: stat, "reqType": "webhook"})
	statusCode := http.StatusBadRequest
	if code != 0 {
		statusCode = code
	}
	pkgLogger.Infof("IP: %s -- %s -- Response: %d, %s", misc.GetIPFromReq(r), r.URL.Path, code, reason)
	http.Error(w, reason, statusCode)
	webhook.gwHandle.IncrementAckCount(1)
}

func (webhook *HandleT) RequestHandler(w http.ResponseWriter, r *http.Request) {
	pkgLogger.LogRequest(r)
	webhook.gwHandle.IncrementRecvCount(1)
	atomic.AddUint64(&webhook.recvCount, 1)

	writeKey, ok := parseWriteKey(r)
	if !ok {
		webhook.failRequest(w, r, response.GetStatus(response.NoWriteKeyInQueryParams), response.GetErrorStatusCode(response.NoWriteKeyInQueryParams), "noWriteKey")
		atomic.AddUint64(&webhook.ackCount, 1)
		return
	}

	sourceDefName, ok := webhook.gwHandle.GetWebhookSourceDefName(writeKey)
	if !ok {
		webhook.failRequest(w, r, response.GetStatus(response.InvalidWriteKey), response.GetErrorStatusCode(response.InvalidWriteKey), writeKey)
		atomic.AddUint64(&webhook.ackCount, 1)
		return
	}

	var postFrom url.Values
	var multipartForm *multipart.Form

	if r.Method == "GET" {
		return
	}
	contentType := r.Header.Get("Content-Type")
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		if err := r.ParseForm(); err != nil {
			webhook.failRequest(w, r, response.GetStatus(response.ErrorInParseForm), response.GetErrorStatusCode(response.ErrorInParseForm), "couldNotParseForm")
			atomic.AddUint64(&webhook.ackCount, 1)
			return
		}
		postFrom = r.PostForm
	} else if strings.Contains(strings.ToLower(contentType), "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			webhook.failRequest(w, r, response.GetStatus(response.ErrorInParseMultiform), response.GetErrorStatusCode(response.ErrorInParseMultiform), "couldNotParseMultiform")
			atomic.AddUint64(&webhook.ackCount, 1)
			return
		}
		multipartForm = r.MultipartForm
	}

	var jsonByte []byte
	var err error

	if r.MultipartForm != nil {
		jsonByte, err = json.Marshal(multipartForm)
		if err != nil {
			webhook.failRequest(w, r, response.GetStatus(response.ErrorInMarshal), response.GetErrorStatusCode(response.ErrorInMarshal), "couldNotMarshal")
			atomic.AddUint64(&webhook.ackCount, 1)
			return
		}
	} else if len(postFrom) != 0 {
		jsonByte, err = json.Marshal(postFrom)
		if err != nil {
			webhook.failRequest(w, r, response.GetStatus(response.ErrorInMarshal), response.GetErrorStatusCode(response.ErrorInMarshal), "couldNotMarshal")
			atomic.AddUint64(&webhook.ackCount, 1)
			return
		}
	}

	if len(jsonByte) != 0 {
		r.Body = io.NopCloser(bytes.NewReader(jsonByte))
		r.Header.Set("Content-Type", "application/json")
	}

	done := make(chan transformerResponse)
	req := webhookT{request: r, writer: w, done: done, sourceType: sourceDefName, writeKey: writeKey}
	webhook.requestQ[sourceDefName] <- &req

	// Wait for batcher process to be done
	resp := <-done
	webhook.gwHandle.IncrementAckCount(1)
	atomic.AddUint64(&webhook.ackCount, 1)
	webhook.gwHandle.TrackRequestMetrics(resp.Err)

	if resp.Err != "" {
		code := http.StatusBadRequest
		if resp.StatusCode != 0 {
			code = resp.StatusCode
		}
		pkgLogger.Infof("IP: %s -- %s -- Response: %d, %s", misc.GetIPFromReq(r), r.URL.Path, code, resp.Err)
		http.Error(w, resp.Err, code)
		return
	}

	payload := []byte(response.Ok)
	if resp.OutputToSource != nil {
		payload = resp.OutputToSource.Body
		w.Header().Set("Content-Type", resp.OutputToSource.ContentType)
	}
	pkgLogger.Debugf("IP: %s -- %s -- Response: 200, %s", misc.GetIPFromReq(r), r.URL.Path, response.GetStatus(response.Ok))
	_, _ = w.Write(payload)
}

func (webhook *HandleT) batchRequests(sourceDef string) {
	reqBuffer := make([]*webhookT, 0)
	timeout := time.After(webhookBatchTimeout)
	for {
		select {
		case req, hasMore := <-webhook.requestQ[sourceDef]:
			if !hasMore {
				if len(reqBuffer) > 0 {
					// If there are requests in the buffer, send them to the batcher
					breq := batchWebhookT{batchRequest: reqBuffer, sourceType: sourceDef}
					webhook.batchRequestQ <- &breq
				}
				return
			}

			// Append to request buffer
			reqBuffer = append(reqBuffer, req)
			if len(reqBuffer) == maxWebhookBatchSize {
				breq := batchWebhookT{batchRequest: reqBuffer, sourceType: sourceDef}
				webhook.batchRequestQ <- &breq
				reqBuffer = make([]*webhookT, 0)
			}
		case <-timeout:
			timeout = time.After(webhookBatchTimeout)
			if len(reqBuffer) > 0 {
				breq := batchWebhookT{batchRequest: reqBuffer, sourceType: sourceDef}
				webhook.batchRequestQ <- &breq
				reqBuffer = make([]*webhookT, 0)
			}
		}
	}
}

// TODO : return back immediately for blank request body. its waiting till timeout
func (bt *batchWebhookTransformerT) batchTransformLoop() {
	for breq := range bt.webhook.batchRequestQ {
		payloadArr := [][]byte{}
		webRequests := []*webhookT{}
		for _, req := range breq.batchRequest {
			body, err := io.ReadAll(req.request.Body)
			req.request.Body.Close()

			if err != nil {
				req.done <- transformerResponse{Err: response.GetStatus(response.RequestBodyReadFailed)}
				continue
			}

			if misc.ContainsString(sourceListForParsingParams, strings.ToLower(breq.sourceType)) {
				queryParams := req.request.URL.Query()
				paramsBytes, err := json.Marshal(queryParams)
				if err != nil {
					req.done <- transformerResponse{Err: response.GetStatus(response.ErrorInMarshal)}
					continue
				}

				closingBraceIdx := bytes.LastIndexByte(body, '}')
				appendData := []byte(`, "query_parameters": `)
				appendData = append(appendData, paramsBytes...)
				body = append(body[:closingBraceIdx], appendData...)
				body = append(body, '}')
			}

			if !json.Valid(body) {
				req.done <- transformerResponse{Err: response.GetStatus(response.InvalidJSON)}
				continue
			}

			payloadArr = append(payloadArr, body)
			webRequests = append(webRequests, req)
		}

		if len(payloadArr) == 0 {
			continue
		}

		if _, ok := bt.stats.sourceStats[breq.sourceType]; !ok {
			bt.stats.sourceStats[breq.sourceType] = newWebhookStat(breq.sourceType)
		}

		// stats
		bt.stats.sourceStats[breq.sourceType].numEvents.Count(len(payloadArr))
		bt.stats.sourceStats[breq.sourceType].sourceTransform.Start()

		batchResponse := bt.transform(payloadArr, breq.sourceType)

		// stats
		bt.stats.sourceStats[breq.sourceType].sourceTransform.End()

		if batchResponse.batchError == nil && len(batchResponse.responses) != len(payloadArr) {
			batchResponse.batchError = errors.New("webhook batchtransform response events size does not equal sent events size")
			pkgLogger.Errorf("%w", batchResponse.batchError)
		}
		if batchResponse.batchError != nil {
			statusCode := http.StatusInternalServerError
			if batchResponse.statusCode != 0 {
				statusCode = batchResponse.statusCode
			}
			for _, req := range breq.batchRequest {
				req.done <- transformerResponse{StatusCode: statusCode, Err: batchResponse.batchError.Error()}
			}
			continue
		}

		bt.stats.sourceStats[breq.sourceType].numOutputEvents.Count(len(batchResponse.responses))

		for idx, resp := range batchResponse.responses {
			webRequest := webRequests[idx]
			if resp.Err == "" && resp.Output != nil {
				outputPayload, err := json.Marshal(resp.Output)
				if err != nil {
					webRequest.done <- bt.markRepsonseFail(response.SourceTransformerInvalidOutputFormatInResponse)
					continue
				}
				errorMessage := bt.webhook.enqueueInGateway(webRequest, outputPayload)
				if errorMessage != "" {
					webRequest.done <- transformerResponse{Err: errorMessage}
					continue
				}
			}
			webRequest.done <- resp
		}
	}
}

func (webhook *HandleT) enqueueInGateway(req *webhookT, payload []byte) string {
	// replace body with transformed event (it comes in a batch format)
	req.request.Body = io.NopCloser(bytes.NewReader(payload))
	// set write key in basic auth header
	req.request.SetBasicAuth(req.writeKey, "")
	payload, err := io.ReadAll(req.request.Body)
	req.request.Body.Close()
	if err != nil {
		return err.Error()
	}
	return webhook.gwHandle.ProcessWebRequest(&req.writer, req.request, "batch", payload, req.writeKey)
}

func (webhook *HandleT) Register(name string) {
	if _, ok := webhook.requestQ[name]; !ok {
		webhook.requestQ[name] = make(chan *webhookT)

		webhook.batchRequestsWg.Add(1)
		go (func() {
			defer webhook.batchRequestsWg.Done()
			webhook.batchRequests(name)
		})()
	}
}

func (webhook *HandleT) Shutdown() error {
	webhook.backgroundCancel()
	for _, q := range webhook.requestQ {
		close(q)
	}
	webhook.batchRequestsWg.Wait()
	close(webhook.batchRequestQ)

	return webhook.backgroundWait()
}

// TODO: Check if correct
func newWebhookStat(sourceType string) *webhookSourceStatT {
	tags := map[string]string{
		"sourceType": sourceType,
	}
	numEvents := stats.NewTaggedStat("webhook_num_events", stats.CountType, tags)
	numOutputEvents := stats.NewTaggedStat("webhook_num_output_events", stats.CountType, tags)
	sourceTransform := stats.NewTaggedStat("webhook_dest_transform", stats.TimerType, tags)
	return &webhookSourceStatT{
		id:              sourceType,
		numEvents:       numEvents,
		numOutputEvents: numOutputEvents,
		sourceTransform: sourceTransform,
	}
}

func (webhook *HandleT) printStats(ctx context.Context) {
	var lastRecvCount, lastackCount uint64
	for {
		if lastRecvCount != webhook.recvCount || lastackCount != webhook.ackCount {
			lastRecvCount = webhook.recvCount
			lastackCount = webhook.ackCount
			pkgLogger.Debug("Webhook Recv/Ack ", webhook.recvCount, webhook.ackCount)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
}
