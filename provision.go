// +build !lambdabinary

package sparta

import (
	"archive/zip"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	spartaCF "github.com/mweagle/Sparta/aws/cloudformation"
	spartaS3 "github.com/mweagle/Sparta/aws/s3"
	spartaZip "github.com/mweagle/Sparta/zip"

	"github.com/mweagle/cloudformationresources"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/aws/aws-sdk-go/service/s3"
	gocf "github.com/crewjam/go-cloudformation"
	spartaAWS "github.com/mweagle/Sparta/aws"
)

////////////////////////////////////////////////////////////////////////////////
// CONSTANTS
////////////////////////////////////////////////////////////////////////////////
func spartaTagName(baseKey string) string {
	return fmt.Sprintf("io:gosparta:%s", baseKey)
}

// SpartaTagHomeKey is the keyname used in the CloudFormation Output
// that stores the Sparta home URL.
// @enum OutputKey
var SpartaTagHomeKey = spartaTagName("home")

// SpartaTagVersionKey is the keyname used in the CloudFormation Output
// that stores the Sparta version used to provision/update the service.
// @enum OutputKey
var SpartaTagVersionKey = spartaTagName("version")

// SpartaTagBuildIDKey is the keyname used in the CloudFormation Output
// that stores the user-supplied or automatically generated BuildID
// for this run
var SpartaTagBuildIDKey = spartaTagName("buildId")

// SpartaTagBuildTagsKey is the keyname used in the CloudFormation Output
// that stores the optional user-supplied golang build tags
var SpartaTagBuildTagsKey = spartaTagName("buildTags")

// The basename of the scripts that are embedded into CONSTANTS.go
// by `esc` during the generate phase.  In order to export these, there
// MUST be a corresponding PROXIED_MODULES entry for the base filename
// in resources/index.js
var customResourceScripts = []string{"sparta_utils.js",
	"golang-constants.json"}

var golangCustomResourceTypes = []string{
	cloudformationresources.SESLambdaEventSource,
	cloudformationresources.S3LambdaEventSource,
	cloudformationresources.SNSLambdaEventSource,
	cloudformationresources.CloudWatchLogsLambdaEventSource,
	cloudformationresources.ZipToS3Bucket,
}

// The relative path of the custom scripts that is used
// to create the filename relative path when creating the custom archive
const provisioningResourcesRelPath = "/resources/provision"

// Represents data associated with provisioning the S3 Site iff defined
type s3SiteContext struct {
	s3Site             *S3Site
	s3SiteLambdaZipKey string
}

// Type of a workflow step.  Each step is responsible
// for returning the next step or an error if the overall
// workflow should stop.
type workflowStep func(ctx *workflowContext) (workflowStep, error)

////////////////////////////////////////////////////////////////////////////////
// Workflow context
// The workflow context is created by `provision` and provided to all
// functions that constitute the provisioning workflow.
type workflowContext struct {
	// Is this is a -dry-run?
	noop bool
	// Canonical basename of the service.  Also used as the CloudFormation
	// stack name
	serviceName string
	// Service description
	serviceDescription string
	// The slice of Lambda functions that constitute the service
	lambdaAWSInfos []*LambdaAWSInfo
	// Optional APIGateway definition to associate with this service
	api *API
	// Optional S3 site data to provision together with this service
	s3SiteContext *s3SiteContext
	// CloudFormation Template
	cfTemplate *gocf.Template
	// Cached IAM role name map.  Used to support dynamic and static IAM role
	// names.  Static ARN role names are checked for existence via AWS APIs
	// prior to CloudFormation provisioning.
	lambdaIAMRoleNameMap map[string]*gocf.StringExpr
	// The user-supplied S3 bucket where service artifacts should be posted.
	s3Bucket string
	// The user-supplied or automatically generated BuildID
	buildID string
	// Optional user-supplied build tags
	buildTags string
	// The time when we started s.t. we can filter stack events
	buildTime time.Time
	// The programmatically determined S3 item key for this service's cloudformation
	// definition.
	s3LambdaZipKey string
	// AWS Session to be used for all API calls made in the process of provisioning
	// this service.
	awsSession *session.Session
	// IO writer for autogenerated template results
	templateWriter io.Writer
	// User supplied workflow hooks
	workflowHooks *WorkflowHooks
	// Context to pass between workflow operations
	workflowHooksContext map[string]interface{}
	// Preconfigured logger
	logger *logrus.Logger
	// Optional rollback functions that workflow steps may append to if they
	// have made mutations during provisioning.
	rollbackFunctions []spartaS3.RollbackFunction
}

// Register a rollback function in the event that the provisioning
// function failed.
func (ctx *workflowContext) registerRollback(userFunction spartaS3.RollbackFunction) {
	if nil == ctx.rollbackFunctions || len(ctx.rollbackFunctions) <= 0 {
		ctx.rollbackFunctions = make([]spartaS3.RollbackFunction, 0)
	}
	ctx.rollbackFunctions = append(ctx.rollbackFunctions, userFunction)
}

// Run any provided rollback functions
func (ctx *workflowContext) rollback() {
	// Run each cleanup function concurrently.  If there's an error
	// all we're going to do is log it as a warning, since at this
	// point there's nothing to do...
	var wg sync.WaitGroup
	wg.Add(len(ctx.rollbackFunctions))

	// Include the user defined rollback if there is one...
	if ctx.workflowHooks != nil && ctx.workflowHooks.Rollback != nil {
		wg.Add(1)
		go func(hook RollbackHook, context map[string]interface{},
			serviceName string,
			awsSession *session.Session,
			noop bool,
			logger *logrus.Logger) {
			// Decrement the counter when the goroutine completes.
			defer wg.Done()
			hook(context, serviceName, awsSession, noop, logger)
		}(ctx.workflowHooks.Rollback,
			ctx.workflowHooksContext,
			ctx.serviceName,
			ctx.awsSession,
			ctx.noop,
			ctx.logger)
	}

	ctx.logger.WithFields(logrus.Fields{
		"RollbackCount": len(ctx.rollbackFunctions),
	}).Info("Invoking rollback functions")

	for _, eachCleanup := range ctx.rollbackFunctions {
		go func(cleanupFunc spartaS3.RollbackFunction, goLogger *logrus.Logger) {
			// Decrement the counter when the goroutine completes.
			defer wg.Done()
			// Fetch the URL.
			err := cleanupFunc(goLogger)
			if nil != err {
				ctx.logger.WithFields(logrus.Fields{
					"Error": err,
				}).Warning("Failed to cleanup resource")
			}
		}(eachCleanup, ctx.logger)
	}
	wg.Wait()
}

////////////////////////////////////////////////////////////////////////////////
// Private - START
//

// Encapsulate calling a workflow hook
func callWorkflowHook(hook WorkflowHook, ctx *workflowContext) error {
	if nil == hook {
		return nil
	}
	// Run the hook
	hookName := runtime.FuncForPC(reflect.ValueOf(hook).Pointer()).Name()
	ctx.logger.WithFields(logrus.Fields{
		"WorkflowHook":        hookName,
		"WorkflowHookContext": ctx.workflowHooksContext,
	}).Info("Calling WorkflowHook")

	return hook(ctx.workflowHooksContext,
		ctx.serviceName,
		ctx.s3Bucket,
		ctx.buildID,
		ctx.awsSession,
		ctx.noop,
		ctx.logger)
}

// Create a temporary file in the current working directory
func temporaryFile(name string) (*os.File, error) {
	workingDir, err := os.Getwd()
	if nil != err {
		return nil, err
	}
	tmpFile, err := ioutil.TempFile(workingDir, name)
	if err != nil {
		return nil, errors.New("Failed to create temporary file")
	}
	return tmpFile, nil
}

func runOSCommand(cmd *exec.Cmd, logger *logrus.Logger) error {
	logger.WithFields(logrus.Fields{
		"Arguments": cmd.Args,
		"Dir":       cmd.Dir,
		"Path":      cmd.Path,
		"Env":       cmd.Env,
	}).Debug("Running Command")
	outputWriter := logger.Writer()
	defer outputWriter.Close()
	cmd.Stdout = outputWriter
	cmd.Stderr = outputWriter
	return cmd.Run()
}

// Ensure that the S3 bucket we're using for archives has an object expiration policy.  The
// uploaded archives otherwise will be orphaned in S3 since the template can't manage the
// associated resources
func ensureExpirationPolicy(awsSession *session.Session, S3Bucket string, noop bool, logger *logrus.Logger) error {
	if noop {
		logger.WithFields(logrus.Fields{
			"BucketName": S3Bucket,
		}).Info("Bypassing bucket expiration policy check due to -n/-noop command line argument")
		return nil
	}
	s3Svc := s3.New(awsSession)
	params := &s3.GetBucketLifecycleConfigurationInput{
		Bucket: aws.String(S3Bucket), // Required
	}
	showWarning := false
	resp, err := s3Svc.GetBucketLifecycleConfiguration(params)
	if nil != err {
		showWarning = strings.Contains(err.Error(), "NoSuchLifecycleConfiguration")
		if !showWarning {
			return fmt.Errorf("Failed to fetch S3 Bucket Policy: %s", err.Error())
		}
	} else {
		for _, eachRule := range resp.Rules {
			if *eachRule.Status == s3.ExpirationStatusEnabled {
				showWarning = false
			}
		}
	}
	if showWarning {
		logger.WithFields(logrus.Fields{
			"Bucket":    S3Bucket,
			"Reference": "http://docs.aws.amazon.com/AmazonS3/latest/dev/how-to-set-lifecycle-configuration-intro.html",
		}).Warning("Bucket should have ObjectExpiration lifecycle enabled.")
	} else {
		logger.WithFields(logrus.Fields{
			"Bucket": S3Bucket,
			"Rules":  resp.Rules,
		}).Debug("Bucket lifecycle configuration")
	}
	return nil
}

// Upload a local file to S3.  Returns the s3 keyname of the
// uploaded item, or an error
func uploadLocalFileToS3(localPath string,
	awsSession *session.Session,
	S3Bucket string,
	S3KeyPrefix string,
	noop bool,
	logger *logrus.Logger) (string, error) {

	// Query the S3 bucket for the bucket policies.  The bucket _should_ have ObjectExpiration,
	// otherwise we're just going to orphan our binaries...
	err := ensureExpirationPolicy(awsSession, S3Bucket, noop, logger)
	if nil != err {
		return "", fmt.Errorf("Failed to ensure bucket policies: %s", err.Error())
	}

	// Ensure the local file is deleted
	defer func() {
		err = os.Remove(localPath)
		if nil != err {
			logger.WithFields(logrus.Fields{
				"Path":  localPath,
				"Error": err,
			}).Warn("Failed to delete local file")
		}
	}()

	keyName := path.Join(S3KeyPrefix, filepath.Base(localPath))
	if noop {
		logger.WithFields(logrus.Fields{
			"Bucket": S3Bucket,
			"Key":    keyName,
		}).Info("Bypassing S3 upload due to -n/-noop command line argument")
	} else {
		_, uploadURLErr := spartaS3.UploadLocalFileToS3(localPath,
			awsSession,
			S3Bucket,
			S3KeyPrefix,
			logger)
		if nil != uploadURLErr {
			return "", uploadURLErr
		}
	}
	return keyName, nil
}

// Private - END
////////////////////////////////////////////////////////////////////////////////

////////////////////////////////////////////////////////////////////////////////
// Workflow steps
////////////////////////////////////////////////////////////////////////////////

// Verify & cache the IAM rolename to ARN mapping
func verifyIAMRoles(ctx *workflowContext) (workflowStep, error) {
	// The map is either a literal Arn from a pre-existing role name
	// or a gocf.RefFunc() value.
	// Don't verify them, just create them...
	ctx.logger.Info("Verifying IAM Lambda execution roles")
	ctx.lambdaIAMRoleNameMap = make(map[string]*gocf.StringExpr, 0)
	svc := iam.New(ctx.awsSession)

	// Assemble all the RoleNames and validate the inline IAMRoleDefinitions
	var allRoleNames []string
	for _, eachLambdaInfo := range ctx.lambdaAWSInfos {
		if "" != eachLambdaInfo.RoleName {
			allRoleNames = append(allRoleNames, eachLambdaInfo.RoleName)
		}
		// Custom resources?
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if "" != eachCustomResource.roleName {
				allRoleNames = append(allRoleNames, eachCustomResource.roleName)
			}
		}

		// Validate the IAMRoleDefinitions associated
		if nil != eachLambdaInfo.RoleDefinition {
			logicalName := eachLambdaInfo.RoleDefinition.logicalName(ctx.serviceName, eachLambdaInfo.lambdaFnName)
			_, exists := ctx.lambdaIAMRoleNameMap[logicalName]
			if !exists {
				// Insert it into the resource creation map and add
				// the "Ref" entry to the hashmap
				ctx.cfTemplate.AddResource(logicalName,
					eachLambdaInfo.RoleDefinition.toResource(eachLambdaInfo.EventSourceMappings, eachLambdaInfo.Options, ctx.logger))

				ctx.lambdaIAMRoleNameMap[logicalName] = gocf.GetAtt(logicalName, "Arn")
			}
		}

		// And the custom resource IAMRoles as well...
		for _, eachCustomResource := range eachLambdaInfo.customResources {
			if nil != eachCustomResource.roleDefinition {
				customResourceLogicalName := eachCustomResource.roleDefinition.logicalName(ctx.serviceName,
					eachCustomResource.userFunctionName)

				_, exists := ctx.lambdaIAMRoleNameMap[customResourceLogicalName]
				if !exists {
					ctx.cfTemplate.AddResource(customResourceLogicalName,
						eachCustomResource.roleDefinition.toResource(nil, eachCustomResource.options, ctx.logger))
					ctx.lambdaIAMRoleNameMap[customResourceLogicalName] = gocf.GetAtt(customResourceLogicalName, "Arn")
				}
			}
		}
	}

	// Then check all the RoleName literals
	for _, eachRoleName := range allRoleNames {
		_, exists := ctx.lambdaIAMRoleNameMap[eachRoleName]
		if !exists {
			// Check the role
			params := &iam.GetRoleInput{
				RoleName: aws.String(eachRoleName),
			}
			ctx.logger.Debug("Checking IAM RoleName: ", eachRoleName)
			resp, err := svc.GetRole(params)
			if err != nil {
				ctx.logger.Error(err.Error())
				return nil, err
			}
			// Cache it - we'll need it later when we create the
			// CloudFormation template which needs the execution Arn (not role)
			ctx.lambdaIAMRoleNameMap[eachRoleName] = gocf.String(*resp.Role.Arn)
		}
	}

	ctx.logger.WithFields(logrus.Fields{
		"Count": len(ctx.lambdaIAMRoleNameMap),
	}).Info("IAM roles verified")

	return createPackageStep(), nil
}

// Return a string representation of a JS function call that can be exposed
// to AWS Lambda
func createNewNodeJSProxyEntry(lambdaInfo *LambdaAWSInfo, logger *logrus.Logger) string {
	logger.WithFields(logrus.Fields{
		"FunctionName": lambdaInfo.lambdaFnName,
	}).Info("Registering Sparta function")

	// We do know the CF resource name here - could write this into
	// index.js and expose a GET localhost:9000/lambdaMetadata
	// which wraps up DescribeStackResource for the running
	// lambda function
	primaryEntry := fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		lambdaInfo.jsHandlerName(),
		lambdaInfo.lambdaFnName)
	return primaryEntry
}

func createUserCustomResourceEntry(customResource *customResourceInfo, logger *logrus.Logger) string {
	// The resource name is a :: delimited one, so let's sanitize that
	// to make it a valid JS identifier

	logger.WithFields(logrus.Fields{
		"UserFunction":       customResource.userFunctionName,
		"NodeJSFunctionName": customResource.jsHandlerName(),
	}).Debug("Registering User CustomResource function")

	primaryEntry := fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		customResource.jsHandlerName(),
		customResource.userFunctionName)
	return primaryEntry
}

func createNewSpartaCustomResourceEntry(resourceName string, logger *logrus.Logger) string {
	// The resource name is a :: delimited one, so let's sanitize that
	// to make it a valid JS identifier
	jsName := javascriptExportNameForCustomResourceType(resourceName)
	logger.WithFields(logrus.Fields{
		"Resource":           resourceName,
		"NodeJSFunctionName": jsName,
	}).Debug("Registering Sparta CustomResource function")

	primaryEntry := fmt.Sprintf("exports[\"%s\"] = createForwarder(\"/%s\");\n",
		jsName,
		resourceName)
	return primaryEntry
}

func logFilesize(message string, filePath string, logger *logrus.Logger) {
	// Binary size
	stat, err := os.Stat(filePath)
	if err == nil {
		logger.WithFields(logrus.Fields{
			"KB": stat.Size() / 1024,
			"MB": stat.Size() / (1024 * 1024),
		}).Info(message)
	}
}

func buildGoBinary(executableOutput string, buildTags string, logger *logrus.Logger) error {
	// Go generate
	cmd := exec.Command("go", "generate")
	if logger.Level == logrus.DebugLevel {
		cmd = exec.Command("go", "generate", "-v", "-x")
	}
	cmd.Env = os.Environ()
	commandString := fmt.Sprintf("%s", cmd.Args)
	logger.Info(fmt.Sprintf("Running `%s`", strings.Trim(commandString, "[]")))
	goGenerateErr := runOSCommand(cmd, logger)
	if nil != goGenerateErr {
		return goGenerateErr
	}

	// TODO: Smaller binaries via linker flags
	// Ref: https://blog.filippo.io/shrink-your-go-binaries-with-this-one-weird-trick/
	allBuildTags := fmt.Sprintf("lambdabinary %s", buildTags)
	cmd = exec.Command("go", "build", "-o", executableOutput, "-tags", allBuildTags, ".")
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, "GOOS=linux", "GOARCH=amd64")
	logger.WithFields(logrus.Fields{
		"Name": executableOutput,
	}).Info("Compiling binary")
	return runOSCommand(cmd, logger)
}

func writeNodeJSShim(serviceName string,
	executableOutput string,
	lambdaAWSInfos []*LambdaAWSInfo,
	zipWriter *zip.Writer,
	logger *logrus.Logger) error {

	// Add the string literal adapter, which requires us to add exported
	// functions to the end of index.js.  These NodeJS exports will be
	// linked to the AWS Lambda NodeJS function name, and are basically
	// automatically generated pass through proxies to the golang HTTP handler.
	nodeJSWriter, err := zipWriter.Create("index.js")
	if err != nil {
		return errors.New("Failed to create ZIP entry: index.js")
	}
	nodeJSSource := _escFSMustString(false, "/resources/index.js")
	nodeJSSource += "\n// DO NOT EDIT - CONTENT UNTIL EOF IS AUTOMATICALLY GENERATED\n"

	handlerNames := make(map[string]bool, 0)
	for _, eachLambda := range lambdaAWSInfos {
		if _, exists := handlerNames[eachLambda.jsHandlerName()]; !exists {
			nodeJSSource += createNewNodeJSProxyEntry(eachLambda, logger)
			handlerNames[eachLambda.jsHandlerName()] = true
		}

		// USER DEFINED RESOURCES
		for _, eachCustomResource := range eachLambda.customResources {
			if _, exists := handlerNames[eachCustomResource.jsHandlerName()]; !exists {
				nodeJSSource += createUserCustomResourceEntry(eachCustomResource, logger)
				handlerNames[eachCustomResource.jsHandlerName()] = true
			}
		}
	}
	// SPARTA CUSTOM RESOURCES
	for _, eachCustomResourceName := range golangCustomResourceTypes {
		nodeJSSource += createNewSpartaCustomResourceEntry(eachCustomResourceName, logger)
	}

	// Finally, replace
	// 	SPARTA_BINARY_NAME = 'Sparta.lambda.amd64';
	// with the service binary name
	nodeJSSource += fmt.Sprintf("SPARTA_BINARY_NAME='%s';\n", executableOutput)
	// And the service name
	nodeJSSource += fmt.Sprintf("SPARTA_SERVICE_NAME='%s';\n", serviceName)
	logger.WithFields(logrus.Fields{
		"index.js": nodeJSSource,
	}).Debug("Dynamically generated NodeJS adapter")

	stringReader := strings.NewReader(nodeJSSource)
	_, copyErr := io.Copy(nodeJSWriter, stringReader)
	return copyErr
}

func writeCustomResources(zipWriter *zip.Writer,
	logger *logrus.Logger) error {
	for _, eachName := range customResourceScripts {
		resourceName := fmt.Sprintf("%s/%s", provisioningResourcesRelPath, eachName)
		resourceContent := _escFSMustString(false, resourceName)
		stringReader := strings.NewReader(resourceContent)
		embedWriter, errCreate := zipWriter.Create(eachName)
		if nil != errCreate {
			return errCreate
		}
		logger.WithFields(logrus.Fields{
			"Name": eachName,
		}).Debug("Script name")

		_, copyErr := io.Copy(embedWriter, stringReader)
		if nil != copyErr {
			return copyErr
		}
	}
	return nil
}

// Build and package the application
func createPackageStep() workflowStep {

	return func(ctx *workflowContext) (workflowStep, error) {

		// PreBuild Hook
		if ctx.workflowHooks != nil {
			preBuildErr := callWorkflowHook(ctx.workflowHooks.PreBuild, ctx)
			if nil != preBuildErr {
				return nil, preBuildErr
			}
		}
		sanitizedServiceName := sanitizedName(ctx.serviceName)
		executableOutput := fmt.Sprintf("%s.lambda.amd64", sanitizedServiceName)
		buildErr := buildGoBinary(executableOutput, ctx.buildTags, ctx.logger)
		if nil != buildErr {
			return nil, buildErr
		}
		// Cleanup the temporary binary
		defer func() {
			errRemove := os.Remove(executableOutput)
			if nil != errRemove {
				ctx.logger.WithFields(logrus.Fields{
					"File":  executableOutput,
					"Error": errRemove,
				}).Warn("Failed to delete binary")
			}
		}()

		// Binary size
		logFilesize("Executable binary size", executableOutput, ctx.logger)

		// PostBuild Hook
		if ctx.workflowHooks != nil {
			postBuildErr := callWorkflowHook(ctx.workflowHooks.PostBuild, ctx)
			if nil != postBuildErr {
				return nil, postBuildErr
			}
		}

		tmpFile, err := temporaryFile(sanitizedServiceName)
		if err != nil {
			return nil, errors.New("Failed to create temporary file")
		}
		ctx.logger.WithFields(logrus.Fields{
			"TempName": tmpFile.Name(),
		}).Info("Creating ZIP archive for upload")

		lambdaArchive := zip.NewWriter(tmpFile)

		// Archive Hook
		if ctx.workflowHooks != nil && ctx.workflowHooks.Archive != nil {
			archiveErr := ctx.workflowHooks.Archive(ctx.workflowHooksContext,
				ctx.serviceName,
				lambdaArchive,
				ctx.awsSession,
				ctx.noop,
				ctx.logger)
			if nil != archiveErr {
				return nil, archiveErr
			}
		}

		// File info for the binary executable
		readerErr := spartaZip.AddToZip(lambdaArchive,
			executableOutput,
			"",
			ctx.logger)
		if nil != readerErr {
			return nil, readerErr
		}

		// Add the string literal adapter, which requires us to add exported
		// functions to the end of index.js.  These NodeJS exports will be
		// linked to the AWS Lambda NodeJS function name, and are basically
		// automatically generated pass through proxies to the golang HTTP handler.
		shimErr := writeNodeJSShim(ctx.serviceName,
			executableOutput,
			ctx.lambdaAWSInfos,
			lambdaArchive,
			ctx.logger)
		if nil != shimErr {
			return nil, shimErr
		}

		// Next embed the custom resource scripts into the package.
		// TODO - conditionally include custom NodeJS scripts based on service requirement
		ctx.logger.Debug("Embedding CustomResource scripts")
		customResourceErr := writeCustomResources(lambdaArchive, ctx.logger)
		if nil != customResourceErr {
			return nil, customResourceErr
		}
		archiveCloseErr := lambdaArchive.Close()
		if nil != archiveCloseErr {
			return nil, archiveCloseErr
		}
		tempfileCloseErr := tmpFile.Close()
		if nil != tempfileCloseErr {
			return nil, tempfileCloseErr
		}
		return createUploadStep(tmpFile.Name()), nil
	}
}

// Given the zipped binary in packagePath, upload the primary code bundle
// and optional S3 site resources iff they're defined.
func createUploadStep(packagePath string) workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		var uploadErrors []error
		var wg sync.WaitGroup

		// We always need to upload the primary binary
		wg.Add(1)
		go func() {
			defer wg.Done()
			logFilesize("Lambda function deployment package size", packagePath, ctx.logger)

			keyName, err := uploadLocalFileToS3(packagePath,
				ctx.awsSession,
				ctx.s3Bucket,
				ctx.serviceName,
				ctx.noop,
				ctx.logger)
			ctx.s3LambdaZipKey = keyName

			if nil != err {
				uploadErrors = append(uploadErrors, err)
			} else if !ctx.noop {
				ctx.registerRollback(spartaS3.CreateS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, ctx.s3LambdaZipKey))
			}
		}()

		// S3 site to compress & upload
		if nil != ctx.s3SiteContext.s3Site {
			wg.Add(1)
			go func() {
				defer wg.Done()

				tempName := fmt.Sprintf("%s-S3Site", ctx.serviceName)
				tmpFile, err := temporaryFile(tempName)
				if err != nil {
					uploadErrors = append(uploadErrors,
						errors.New("Failed to create temporary S3 site archive file"))
					return
				}

				// Add the contents to the Zip file
				zipArchive := zip.NewWriter(tmpFile)
				absResourcePath, err := filepath.Abs(ctx.s3SiteContext.s3Site.resources)
				if nil != err {
					uploadErrors = append(uploadErrors, err)
					return
				}

				ctx.logger.WithFields(logrus.Fields{
					"S3Key":  path.Base(tmpFile.Name()),
					"Source": absResourcePath,
				}).Info("Creating S3Site archive")

				err = spartaZip.AddToZip(zipArchive, absResourcePath, absResourcePath, ctx.logger)
				if nil != err {
					uploadErrors = append(uploadErrors, err)
					return
				}
				zipArchive.Close()
				// Upload it & save the key
				s3SiteLambdaZipKey, err := uploadLocalFileToS3(tmpFile.Name(),
					ctx.awsSession,
					ctx.s3Bucket,
					ctx.serviceName,
					ctx.noop,
					ctx.logger)
				ctx.s3SiteContext.s3SiteLambdaZipKey = s3SiteLambdaZipKey
				if nil != err {
					uploadErrors = append(uploadErrors, err)
				} else if !ctx.noop {
					ctx.registerRollback(spartaS3.CreateS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, ctx.s3SiteContext.s3SiteLambdaZipKey))
				}
			}()
		}
		wg.Wait()

		if len(uploadErrors) > 0 {
			errorText := "Encountered multiple errors during upload:\n"
			for _, eachError := range uploadErrors {
				errorText += fmt.Sprintf("%s%s\n", errorText, eachError.Error())
				return nil, errors.New(errorText)
			}
		}
		return ensureCloudFormationStack(), nil
	}
}

func annotateDiscoveryInfo(template *gocf.Template, logger *logrus.Logger) *gocf.Template {
	for eachResourceID, eachResource := range template.Resources {
		// Only apply this to lambda functions
		if eachResource.Properties.CfnResourceType() == "AWS::Lambda::Function" {

			// Update the metdata with a reference to the output of each
			// depended on item...
			for _, eachDependsKey := range eachResource.DependsOn {
				dependencyOutputs, _ := outputsForResource(template, eachDependsKey, logger)
				if nil != dependencyOutputs && len(dependencyOutputs) != 0 {
					logger.WithFields(logrus.Fields{
						"Resource":  eachDependsKey,
						"DependsOn": eachResource.DependsOn,
						"Outputs":   dependencyOutputs,
					}).Debug("Resource metadata")
					safeMetadataInsert(eachResource, eachDependsKey, dependencyOutputs)
				}
			}
			// Also include standard AWS outputs at a resource level if a lambda
			// needs to self-discover other resources
			safeMetadataInsert(eachResource, TagLogicalResourceID, gocf.String(eachResourceID))
			safeMetadataInsert(eachResource, TagStackRegion, gocf.Ref("AWS::Region"))
			safeMetadataInsert(eachResource, TagStackID, gocf.Ref("AWS::StackId"))
			safeMetadataInsert(eachResource, TagStackName, gocf.Ref("AWS::StackName"))
		}
	}
	return template
}

func applyCloudFormationOperation(ctx *workflowContext) (workflowStep, error) {

	stackTags := map[string]string{
		SpartaTagHomeKey:    "http://gosparta.io",
		SpartaTagVersionKey: SpartaVersion,
		SpartaTagBuildIDKey: ctx.buildID,
	}
	if len(ctx.buildTags) != 0 {
		stackTags[SpartaTagBuildTagsKey] = ctx.buildTags
	}
	// Generate a complete CloudFormation template
	if nil != ctx.templateWriter || ctx.logger.Level <= logrus.DebugLevel {
		cfTemplate, err := json.Marshal(ctx.cfTemplate)
		if err != nil {
			ctx.logger.Error("Failed to Marshal CloudFormation template: ", err.Error())
			return nil, err
		}
		templateBody := string(cfTemplate)
		formatted, formattedErr := json.MarshalIndent(templateBody, "", " ")
		if nil != formattedErr {
			return nil, formattedErr
		}
		ctx.logger.WithFields(logrus.Fields{
			"Body": string(formatted),
		}).Debug("CloudFormation template body")
		if nil != ctx.templateWriter {
			io.WriteString(ctx.templateWriter, string(formatted))
		}
	}

	// Upload the actual CloudFormation template to S3 to maximize the template
	// size limit
	// Ref: http://docs.aws.amazon.com/AWSCloudFormation/latest/APIReference/API_CreateStack.html
	sanitizedServiceName := sanitizedName(ctx.serviceName)
	hash := sha1.New()
	hash.Write([]byte(ctx.buildID))
	s3KeyName := fmt.Sprintf("%s/%s-%s-cf.json",
		ctx.serviceName,
		sanitizedServiceName,
		hex.EncodeToString(hash.Sum(nil)))

	if ctx.noop {
		ctx.logger.WithFields(logrus.Fields{
			"Bucket": ctx.s3Bucket,
			"Key":    s3KeyName,
		}).Info("Bypassing template upload & creation due to -n/-noop command line argument")
	} else {
		ctx.logger.Info("Uploading CloudFormation template")
		stack, stackErr := spartaCF.ConvergeStackState(ctx.serviceName,
			ctx.cfTemplate,
			ctx.s3Bucket,
			s3KeyName,
			stackTags,
			ctx.buildTime,
			ctx.awsSession,
			ctx.logger)
		if nil != stackErr {
			return nil, stackErr
		}
		ctx.registerRollback(spartaS3.CreateS3RollbackFunc(ctx.awsSession, ctx.s3Bucket, s3KeyName))
		ctx.logger.WithFields(logrus.Fields{
			"StackName":    *stack.StackName,
			"StackId":      *stack.StackId,
			"CreationTime": *stack.CreationTime,
		}).Info("Stack provisioned")
	}
	return nil, nil
}

func ensureCloudFormationStack() workflowStep {
	return func(ctx *workflowContext) (workflowStep, error) {
		// PreMarshall Hook
		if ctx.workflowHooks != nil {
			preMarshallErr := callWorkflowHook(ctx.workflowHooks.PreMarshall, ctx)
			if nil != preMarshallErr {
				return nil, preMarshallErr
			}
		}

		for _, eachEntry := range ctx.lambdaAWSInfos {
			err := eachEntry.export(ctx.serviceName,
				ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.buildID,
				ctx.lambdaIAMRoleNameMap,
				ctx.cfTemplate,
				ctx.workflowHooksContext,
				ctx.logger)
			if nil != err {
				return nil, err
			}
		}
		// If there's an API gateway definition, include the resources that provision it. Since this export will likely
		// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator, pass that to the S3Site
		// if it's defined, and then merge it with the normal output map.
		apiGatewayTemplate := gocf.NewTemplate()

		if nil != ctx.api {
			err := ctx.api.export(
				ctx.serviceName,
				ctx.awsSession,
				ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.lambdaIAMRoleNameMap,
				apiGatewayTemplate,
				ctx.noop,
				ctx.logger)
			if nil == err {
				err = safeMergeTemplates(apiGatewayTemplate, ctx.cfTemplate, ctx.logger)
			}
			if nil != err {
				return nil, fmt.Errorf("Failed to export APIGateway template resources")
			}
		}
		// If there's a Site defined, include the resources the provision it
		if nil != ctx.s3SiteContext.s3Site {
			ctx.s3SiteContext.s3Site.export(ctx.serviceName,
				ctx.s3Bucket,
				ctx.s3LambdaZipKey,
				ctx.s3SiteContext.s3SiteLambdaZipKey,
				apiGatewayTemplate.Outputs,
				ctx.lambdaIAMRoleNameMap,
				ctx.cfTemplate,
				ctx.logger)
		}
		// Service decorator?
		// If there's an API gateway definition, include the resources that provision it. Since this export will likely
		// generate outputs that the s3 site needs, we'll use a temporary outputs accumulator, pass that to the S3Site
		// if it's defined, and then merge it with the normal output map.-
		if nil != ctx.workflowHooks && nil != ctx.workflowHooks.ServiceDecorator {
			hookName := runtime.FuncForPC(reflect.ValueOf(ctx.workflowHooks.ServiceDecorator).Pointer()).Name()
			ctx.logger.WithFields(logrus.Fields{
				"WorkflowHook":        hookName,
				"WorkflowHookContext": ctx.workflowHooksContext,
			}).Info("Calling WorkflowHook")

			serviceTemplate := gocf.NewTemplate()
			decoratorError := ctx.workflowHooks.ServiceDecorator(
				ctx.workflowHooksContext,
				ctx.serviceName,
				serviceTemplate,
				ctx.s3Bucket,
				ctx.buildID,
				ctx.awsSession,
				ctx.noop,
				ctx.logger,
			)
			if nil != decoratorError {
				return nil, decoratorError
			}
			mergeErr := safeMergeTemplates(serviceTemplate, ctx.cfTemplate, ctx.logger)
			if nil != mergeErr {
				return nil, mergeErr
			}
		}
		ctx.cfTemplate = annotateDiscoveryInfo(ctx.cfTemplate, ctx.logger)

		// PostMarshall Hook
		if ctx.workflowHooks != nil {
			postMarshallErr := callWorkflowHook(ctx.workflowHooks.PostMarshall, ctx)
			if nil != postMarshallErr {
				return nil, postMarshallErr
			}
		}
		return applyCloudFormationOperation(ctx)
	}
}

// Provision compiles, packages, and provisions (either via create or update) a Sparta application.
// The serviceName is the service's logical
// identify and is used to determine create vs update operations.  The compilation options/flags are:
//
// 	TAGS:         -tags lambdabinary
// 	ENVIRONMENT:  GOOS=linux GOARCH=amd64
//
// The compiled binary is packaged with a NodeJS proxy shim to manage AWS Lambda setup & invocation per
// http://docs.aws.amazon.com/lambda/latest/dg/authoring-function-in-nodejs.html
//
// The two files are ZIP'd, posted to S3 and used as an input to a dynamically generated CloudFormation
// template (http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/Welcome.html)
// which creates or updates the service state.
//
// More information on golang 1.5's support for vendor'd resources is documented at
//
//  https://docs.google.com/document/d/1Bz5-UB7g2uPBdOx-rw5t9MxJwkfpx90cqG9AFL0JAYo/edit
//  https://medium.com/@freeformz/go-1-5-s-vendor-experiment-fd3e830f52c3#.voiicue1j
//
// type Configuration struct {
//     Val   string
//     Proxy struct {
//         Address string
//         Port    string
//     }
// }
func Provision(noop bool,
	serviceName string,
	serviceDescription string,
	lambdaAWSInfos []*LambdaAWSInfo,
	api *API,
	site *S3Site,
	s3Bucket string,
	buildID string,
	buildTags string,
	templateWriter io.Writer,
	workflowHooks *WorkflowHooks,
	logger *logrus.Logger) error {

	err := validateSpartaPreconditions(lambdaAWSInfos, logger)
	if nil != err {
		return err
	}
	startTime := time.Now()

	ctx := &workflowContext{
		noop:               noop,
		serviceName:        serviceName,
		serviceDescription: serviceDescription,
		lambdaAWSInfos:     lambdaAWSInfos,
		api:                api,
		s3SiteContext: &s3SiteContext{
			s3Site: site,
		},
		cfTemplate:           gocf.NewTemplate(),
		s3Bucket:             s3Bucket,
		buildID:              buildID,
		buildTags:            buildTags,
		buildTime:            time.Now(),
		awsSession:           spartaAWS.NewSession(logger),
		templateWriter:       templateWriter,
		workflowHooks:        workflowHooks,
		workflowHooksContext: make(map[string]interface{}, 0),
		logger:               logger,
	}
	ctx.cfTemplate.Description = serviceDescription

	// Update the context iff it exists
	if nil != workflowHooks && nil != workflowHooks.Context {
		for eachKey, eachValue := range workflowHooks.Context {
			ctx.workflowHooksContext[eachKey] = eachValue
		}
	}

	ctx.logger.WithFields(logrus.Fields{
		"BuildID": buildID,
		"NOOP":    noop,
		"Tags":    ctx.buildTags,
	}).Info("Provisioning service")

	if len(lambdaAWSInfos) <= 0 {
		return errors.New("No lambda functions provided to Sparta.Provision()")
	}

	// Start the workflow
	for step := verifyIAMRoles; step != nil; {
		next, err := step(ctx)
		if err != nil {
			ctx.rollback()
			// Workflow step?
			ctx.logger.Error(err)
			return err
		}
		if next == nil {
			elapsed := time.Since(startTime)
			ctx.logger.WithFields(logrus.Fields{
				"Seconds": fmt.Sprintf("%.f", elapsed.Seconds()),
			}).Info("Elapsed time")
			break
		} else {
			step = next
		}
	}
	return nil
}
