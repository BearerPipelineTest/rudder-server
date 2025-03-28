package lambda

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/golang/mock/gomock"
	mock_lambda "github.com/rudderlabs/rudder-server/mocks/services/streammanager/lambda"
	mock_logger "github.com/rudderlabs/rudder-server/mocks/utils/logger"
	"github.com/rudderlabs/rudder-server/services/streammanager/common"
	"github.com/stretchr/testify/assert"
)

var (
	sampleMessage  = "sample payload"
	sampleFunction = "sample function"
	invocationType = "Event"
)

func TestNewProducer(t *testing.T) {
	destinationConfig := map[string]interface{}{
		"Region":     "us-east-1",
		"IAMRoleARN": "sampleRoleArn",
		"ExternalID": "sampleExternalID",
	}
	timeOut := 10 * time.Second
	producer, err := NewProducer(destinationConfig, common.Opts{Timeout: timeOut})
	assert.Nil(t, err)
	assert.NotNil(t, producer)
	assert.NotNil(t, producer.client)

	// Invalid Region
	destinationConfig = map[string]interface{}{
		"IAMRoleARN": "sampleRoleArn",
		"ExternalID": "sampleExternalID",
	}
	timeOut = 10 * time.Second
	producer, err = NewProducer(destinationConfig, common.Opts{Timeout: timeOut})
	assert.Nil(t, producer)
	assert.Equal(t, "could not find region configuration", err.Error())
}

func TestProduceWithInvalidClient(t *testing.T) {
	producer := &LambdaProducer{}
	sampleEventJson := []byte("{}")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Equal(t, "[Lambda] error :: Could not create client", respMsg)
}

func TestProduceWithInvalidData(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_lambda.NewMockLambdaClient(ctrl)
	producer := &LambdaProducer{client: mockClient}
	mockLogger := mock_logger.NewMockLoggerI(ctrl)
	pkgLogger = mockLogger

	// Invalid input
	sampleEventJson := []byte("invalid json")
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Lambda] error while unmarshalling jsonData")

	// Empty payload
	sampleEventJson, _ = json.Marshal(map[string]interface{}{
		"payload": "",
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Lambda] error :: Invalid payload")

	// Destination Config not present
	sampleEventJson, _ = json.Marshal(map[string]interface{}{
		"payload": sampleMessage,
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Lambda] error :: Invalid destination config")

	// Invalid Destination Config
	sampleDestConfig := map[string]interface{}{}
	sampleEventJson, _ = json.Marshal(map[string]interface{}{
		"payload":    sampleMessage,
		"destConfig": "invalid dest config",
	})
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, sampleDestConfig)
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.Contains(t, respMsg, "[Lambda] error while unmarshalling jsonData")
}

func TestProduceWithServiceResponse(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockClient := mock_lambda.NewMockLambdaClient(ctrl)
	producer := &LambdaProducer{client: mockClient}
	mockLogger := mock_logger.NewMockLoggerI(ctrl)
	pkgLogger = mockLogger

	sampleDestConfig := map[string]interface{}{
		"Lambda":         sampleFunction,
		"InvocationType": invocationType,
	}

	sampleEventJson, _ := json.Marshal(map[string]interface{}{
		"payload":    sampleMessage,
		"destConfig": sampleDestConfig,
	})

	var sampleInput lambda.InvokeInput
	sampleInput.SetFunctionName(sampleFunction)
	sampleInput.SetPayload([]byte(sampleMessage))
	sampleInput.SetInvocationType(invocationType)

	mockClient.
		EXPECT().
		Invoke(&sampleInput).
		Return(&lambda.InvokeOutput{}, nil)
	statusCode, statusMsg, respMsg := producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 200, statusCode)
	assert.Equal(t, "Success", statusMsg)
	assert.NotEmpty(t, respMsg)

	// return general Error
	errorCode := "errorCode"
	mockClient.
		EXPECT().
		Invoke(&sampleInput).
		Return(nil, errors.New(errorCode))
	mockLogger.EXPECT().Errorf(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 500, statusCode)
	assert.Equal(t, "Failure", statusMsg)
	assert.NotEmpty(t, respMsg)

	// return aws error
	mockClient.
		EXPECT().
		Invoke(&sampleInput).
		Return(nil, awserr.NewRequestFailure(
			awserr.New(errorCode, errorCode, errors.New(errorCode)), 400, "request-id",
		))
	mockLogger.EXPECT().Errorf(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(1)
	statusCode, statusMsg, respMsg = producer.Produce(sampleEventJson, map[string]string{})
	assert.Equal(t, 400, statusCode)
	assert.Equal(t, errorCode, statusMsg)
	assert.NotEmpty(t, respMsg)
}
