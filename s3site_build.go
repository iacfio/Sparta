// +build !lambdabinary

package sparta

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3"
	cfCustomResources "github.com/mweagle/Sparta/aws/cloudformation/resources"
	spartaIAM "github.com/mweagle/Sparta/aws/iam"
	gocf "github.com/mweagle/go-cloudformation"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

const (
	// OutputS3SiteURL is the keyname used in the CloudFormation Output
	// that stores the S3 backed static site provisioned with this Sparta application
	// @enum OutputKey
	OutputS3SiteURL = "S3SiteURL"
)

// Create the resource, which will be part of the stack definition and use a CustomResource
// to copy the content.  Which means we need PutItem access to the target Bucket.  Use
// Cloudformation to create a random bucketname:
// http://docs.aws.amazon.com/AWSCloudFormation/latest/UserGuide/aws-properties-s3-bucket.html

// Need to create the S3 target bucket
// http://docs.aws.amazon.com/AWSJavaScriptSDK/latest/AWS/S3.html#putBucketWebsite-property

// export marshals the API data to a CloudFormation compatible representation
func (s3Site *S3Site) export(serviceName string,
	binaryName string,
	S3Bucket string,
	S3Key string,
	S3ResourcesKey string,
	apiGatewayOutputs map[string]*gocf.Output,
	roleNameMap map[string]*gocf.StringExpr,
	template *gocf.Template,
	logger *logrus.Logger) error {

	websiteConfig := s3Site.WebsiteConfiguration
	if nil == websiteConfig {
		websiteConfig = &s3.WebsiteConfiguration{}
	}

	//////////////////////////////////////////////////////////////////////////////
	// 1 - Create the S3 bucket.  The "BucketName" property is empty s.t.
	// AWS will assign a unique one.
	if nil == websiteConfig.ErrorDocument {
		websiteConfig.ErrorDocument = &s3.ErrorDocument{
			Key: aws.String("error.html"),
		}
	}
	if nil == websiteConfig.IndexDocument {
		websiteConfig.IndexDocument = &s3.IndexDocument{
			Suffix: aws.String("index.html"),
		}
	}

	s3WebsiteConfig := &gocf.S3BucketWebsiteConfiguration{
		ErrorDocument: gocf.String(aws.StringValue(websiteConfig.ErrorDocument.Key)),
		IndexDocument: gocf.String(aws.StringValue(websiteConfig.IndexDocument.Suffix)),
	}
	s3Bucket := &gocf.S3Bucket{
		AccessControl:        gocf.String("PublicRead"),
		WebsiteConfiguration: s3WebsiteConfig,
	}
	s3BucketResourceName := s3Site.CloudFormationS3ResourceName()
	cfResource := template.AddResource(s3BucketResourceName, s3Bucket)
	cfResource.DeletionPolicy = "Delete"

	template.Outputs[OutputS3SiteURL] = &gocf.Output{
		Description: "S3 Website URL",
		Value:       gocf.GetAtt(s3BucketResourceName, "WebsiteURL"),
	}

	// Represents the S3 ARN that is provisioned
	s3SiteBucketResourceValue := gocf.Join("",
		gocf.String("arn:aws:s3:::"),
		gocf.Ref(s3BucketResourceName))
	s3SiteBucketAllKeysResourceValue := gocf.Join("",
		gocf.String("arn:aws:s3:::"),
		gocf.Ref(s3BucketResourceName),
		gocf.String("/*"))

	//////////////////////////////////////////////////////////////////////////////
	// 2 - Add a bucket policy to enable anonymous access, as the PublicRead
	// canned ACL doesn't seem to do what is implied.
	// TODO - determine if this is needed or if PublicRead is being misued
	s3SiteBucketPolicy := &gocf.S3BucketPolicy{
		Bucket: gocf.Ref(s3BucketResourceName).String(),
		PolicyDocument: ArbitraryJSONObject{
			"Version": "2012-10-17",
			"Statement": []ArbitraryJSONObject{
				{
					"Sid":    "PublicReadGetObject",
					"Effect": "Allow",
					"Principal": ArbitraryJSONObject{
						"AWS": "*",
					},
					"Action":   "s3:GetObject",
					"Resource": s3SiteBucketAllKeysResourceValue,
				},
			},
		},
	}
	s3BucketPolicyResourceName := stableCloudformationResourceName("S3SiteBucketPolicy")
	template.AddResource(s3BucketPolicyResourceName, s3SiteBucketPolicy)

	//////////////////////////////////////////////////////////////////////////////
	// 3 - Create the IAM role for the lambda function
	// The lambda function needs to download the posted resource content, as well
	// as manage the S3 bucket that hosts the site.
	statements := CommonIAMStatements.Core
	statements = append(statements, spartaIAM.PolicyStatement{
		Action: []string{"s3:ListBucket",
			"s3:ListObjectsPages"},
		Effect:   "Allow",
		Resource: s3SiteBucketResourceValue,
	})
	statements = append(statements, spartaIAM.PolicyStatement{
		Action: []string{"s3:DeleteObject",
			"s3:PutObject",
			"s3:DeleteObjects",
			"s3:DeleteObjects"},
		Effect:   "Allow",
		Resource: s3SiteBucketAllKeysResourceValue,
	})
	statements = append(statements, spartaIAM.PolicyStatement{
		Action: []string{"s3:GetObject"},
		Effect: "Allow",
		Resource: gocf.Join("",
			gocf.String("arn:aws:s3:::"),
			gocf.String(S3Bucket),
			gocf.String("/"),
			gocf.String(S3ResourcesKey)),
	})

	iamPolicyList := gocf.IAMRolePolicyList{}
	iamPolicyList = append(iamPolicyList,
		gocf.IAMRolePolicy{
			PolicyDocument: ArbitraryJSONObject{
				"Version":   "2012-10-17",
				"Statement": statements,
			},
			PolicyName: gocf.String("S3SiteMgmnt"),
		},
	)

	iamS3Role := &gocf.IAMRole{
		AssumeRolePolicyDocument: AssumePolicyDocument,
		Policies:                 &iamPolicyList,
	}

	iamRoleName := stableCloudformationResourceName("S3SiteIAMRole")
	cfResource = template.AddResource(iamRoleName, iamS3Role)
	cfResource.DependsOn = append(cfResource.DependsOn, s3BucketResourceName)
	iamRoleRef := gocf.GetAtt(iamRoleName, "Arn")

	// Create the IAM role and CustomAction handler to do the work

	//////////////////////////////////////////////////////////////////////////////
	// 4 - Create the lambda function definition that executes with the
	// dynamically provisioned IAM policy.  This is similar to what happens in
	// ensureCustomResourceHandler, but due to the more complex IAM rules
	// there's a bit of duplication
	//	handlerName := lambdaExportNameForCustomResourceType(cloudformationresources.ZipToS3Bucket)
	logger.WithFields(logrus.Fields{
		"CustomResourceType": cfCustomResources.ZipToS3Bucket,
	}).Debug("Sparta CloudFormation custom resource handler info")

	lambdaFunctionName := awsLambdaFunctionName(cfCustomResources.ZipToS3Bucket)

	lambdaEnv, lambdaEnvErr := lambdaFunctionEnvironment(nil,
		cfCustomResources.ZipToS3Bucket,
		nil,
		logger)
	if lambdaEnvErr != nil {
		return errors.Wrapf(lambdaEnvErr, "Failed to create S3 site resource")
	}
	customResourceHandlerDef := gocf.LambdaFunction{
		Code: &gocf.LambdaFunctionCode{
			S3Bucket: gocf.String(S3Bucket),
			S3Key:    gocf.String(S3Key),
		},
		Description: gocf.String(customResourceDescription(serviceName,
			"S3 static site")),
		Handler:      gocf.String(binaryName),
		Role:         iamRoleRef,
		Runtime:      gocf.String(GoLambdaVersion),
		MemorySize:   gocf.Integer(256),
		Timeout:      gocf.Integer(180),
		FunctionName: lambdaFunctionName.String(),
		Environment:  lambdaEnv,
	}
	lambdaResourceName := stableCloudformationResourceName("S3SiteCreator")
	cfResource = template.AddResource(lambdaResourceName, customResourceHandlerDef)
	cfResource.DependsOn = append(cfResource.DependsOn, s3BucketResourceName, iamRoleName)

	//////////////////////////////////////////////////////////////////////////////
	// 5 - Create the custom resource that invokes the site bootstrapper lambda to
	// actually populate the S3 with content
	customResourceName := CloudFormationResourceName("S3SiteBuilder")
	newResource, err := newCloudFormationResource(cfCustomResources.ZipToS3Bucket, logger)
	if nil != err {
		return errors.Wrapf(err, "Failed to create ZipToS3Bucket CustomResource")
	}
	zipResource, zipResourceOK := newResource.(*cfCustomResources.ZipToS3BucketResource)
	if !zipResourceOK {
		return errors.Errorf("Failed to type assert *cfCustomResources.ZipToS3BucketResource custom resource")
	}
	zipResource.ServiceToken = gocf.GetAtt(lambdaResourceName, "Arn")
	zipResource.SrcKeyName = gocf.String(S3ResourcesKey)
	zipResource.SrcBucket = gocf.String(S3Bucket)
	zipResource.DestBucket = gocf.Ref(s3BucketResourceName).String()

	// Build the manifest data with any output info...
	manifestData := make(map[string]interface{})
	for eachKey, eachOutput := range apiGatewayOutputs {
		manifestData[eachKey] = map[string]interface{}{
			"Description": eachOutput.Description,
			"Value":       eachOutput.Value,
		}
	}
	zipResource.Manifest = manifestData
	cfResource = template.AddResource(customResourceName, zipResource)
	cfResource.DependsOn = append(cfResource.DependsOn, lambdaResourceName, s3BucketResourceName)

	return nil
}

// NewS3Site returns a new S3Site pointer initialized with the
// static resources at the supplied path.  If resources is a directory,
// the contents will be recursively archived and used to populate
// the new S3 bucket.
func NewS3Site(resources string) (*S3Site, error) {
	absPath, err := filepath.Abs(resources)
	if nil != err {
		return nil, err
	}
	_, err = os.Stat(absPath)
	if nil != err {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("Path does not exist: %s", absPath)
		}
	}
	site := &S3Site{
		resources: resources,
	}
	return site, nil
}
