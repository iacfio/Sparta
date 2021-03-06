package sparta

import (
	"context"
	"net/http"

	"github.com/aws/aws-lambda-go/lambdacontext"
	"github.com/sirupsen/logrus"
)

// NOTE: your application MUST use `package main` and define a `main()` function.  The
// example text is to make the documentation compatible with godoc.

func echoAPIGatewayHTTPEvent(ctx context.Context,
	props map[string]interface{}) error {
	lambdaCtx, _ := lambdacontext.FromContext(ctx)
	Logger().WithFields(logrus.Fields{
		"RequestID":  lambdaCtx.AwsRequestID,
		"Properties": props,
	}).Info("Lambda event")
	return nil
}

// Should be main() in your application
func ExampleMain_apiGatewayHTTPSEvent() {

	// Create the MyEchoAPI API Gateway, with stagename /test.  The associated
	// Stage reesource will cause the API to be deployed.
	stage := NewStage("v1")
	apiGateway := NewAPIGateway("MyEchoHTTPAPI", stage)

	// Create a lambda function
	echoAPIGatewayLambdaFn := HandleAWSLambda(LambdaName(echoAPIGatewayHTTPEvent),
		echoAPIGatewayHTTPEvent,
		IAMRoleDefinition{})

	// Associate a URL path component with the Lambda function
	apiGatewayResource, _ := apiGateway.NewResource("/echoHelloWorld", echoAPIGatewayLambdaFn)

	// Associate 1 or more HTTP methods with the Resource.
	method, err := apiGatewayResource.NewMethod("GET", http.StatusOK)
	if err != nil {
		panic("Failed to create NewMethod")
	}
	// Whitelist query parameters that should be passed to lambda function
	method.Parameters["method.request.querystring.myKey"] = true
	method.Parameters["method.request.querystring.myOtherKey"] = true

	// Start
	Main("HelloWorldLambdaHTTPSService", "Description for Hello World HTTPS Lambda", []*LambdaAWSInfo{echoAPIGatewayLambdaFn}, apiGateway, nil)
}
