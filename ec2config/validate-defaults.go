package ec2config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-k8s-tester/pkg/fileutil"
	"github.com/aws/aws-k8s-tester/pkg/logutil"
	"github.com/aws/aws-k8s-tester/pkg/randutil"
	"github.com/aws/aws-k8s-tester/pkg/terminal"
	"github.com/aws/aws-sdk-go/aws/endpoints"
)

// NewDefault returns a default configuration.
//  - empty string creates a non-nil object for pointer-type field
//  - omitting an entire field returns nil value
//  - make sure to check both
func NewDefault() *Config {
	name := fmt.Sprintf("ec2-%s-%s", getTS()[:10], randutil.String(12))
	if v := os.Getenv(AWS_K8S_TESTER_EC2_PREFIX + "NAME"); v != "" {
		name = v
	}
	return &Config{
		mu: new(sync.RWMutex),

		Name:      name,
		Partition: endpoints.AwsPartitionID,
		Region:    endpoints.UsWest2RegionID,

		// to be auto-generated
		ConfigPath:                     "",
		RemoteAccessCommandsOutputPath: "",

		LogColor: true,
		LogLevel: logutil.DefaultLogLevel,
		// default, stderr, stdout, or file name
		// log file named with cluster name will be added automatically
		LogOutputs: []string{"stderr"},

		OnFailureDelete:            true,
		OnFailureDeleteWaitSeconds: 120,

		S3BucketName:                    "",
		S3BucketCreate:                  false,
		S3BucketCreateKeep:              false,
		S3BucketLifecycleExpirationDays: 0,

		RoleCreate:                 true,
		VPCCreate:                  true,
		RemoteAccessKeyCreate:      true,
		RemoteAccessPrivateKeyPath: filepath.Join(os.TempDir(), randutil.String(10)+".insecure.key"),

		ASGsFetchLogs: true,
		ASGs: map[string]ASG{
			name + "-asg": {
				Name:                               name + "-asg",
				SSMDocumentCreate:                  false,
				SSMDocumentName:                    "",
				SSMDocumentCommands:                "",
				SSMDocumentExecutionTimeoutSeconds: 3600,
				RemoteAccessUserName:               "ec2-user", // for AL2
				AMIType:                            AMITypeAL2X8664,
				ImageID:                            "",
				ImageIDSSMParameter:                "/aws/service/ami-amazon-linux-latest/amzn2-ami-hvm-x86_64-gp2",
				InstanceTypes:                      []string{DefaultNodeInstanceTypeCPU},
				VolumeSize:                         DefaultNodeVolumeSize,
				ASGMinSize:                         1,
				ASGMaxSize:                         1,
				ASGDesiredCapacity:                 1,
			},
		},
	}
}

// ValidateAndSetDefaults returns an error for invalid configurations.
// And updates empty fields with default values.
// At the end, it writes populated YAML to aws-k8s-tester config path.
func (cfg *Config) ValidateAndSetDefaults() error {
	if cfg.mu == nil {
		cfg.mu = new(sync.RWMutex)
	}
	cfg.mu.Lock()
	defer func() {
		cfg.unsafeSync()
		cfg.mu.Unlock()
	}()

	if err := cfg.validateConfig(); err != nil {
		return fmt.Errorf("validateConfig failed [%v]", err)
	}
	if err := cfg.validateASGs(); err != nil {
		return fmt.Errorf("validateASGs failed [%v]", err)
	}

	return nil
}

func (cfg *Config) validateConfig() error {
	if len(cfg.Name) == 0 {
		return errors.New("Name is empty")
	}
	if cfg.Name != strings.ToLower(cfg.Name) {
		return fmt.Errorf("Name %q must be in lower-case", cfg.Name)
	}

	var partition endpoints.Partition
	switch cfg.Partition {
	case endpoints.AwsPartitionID:
		partition = endpoints.AwsPartition()
	case endpoints.AwsCnPartitionID:
		partition = endpoints.AwsCnPartition()
	case endpoints.AwsUsGovPartitionID:
		partition = endpoints.AwsUsGovPartition()
	case endpoints.AwsIsoPartitionID:
		partition = endpoints.AwsIsoPartition()
	case endpoints.AwsIsoBPartitionID:
		partition = endpoints.AwsIsoBPartition()
	default:
		return fmt.Errorf("unknown partition %q", cfg.Partition)
	}
	regions := partition.Regions()
	if _, ok := regions[cfg.Region]; !ok {
		return fmt.Errorf("region %q for partition %q not found in %+v", cfg.Region, cfg.Partition, regions)
	}

	_, cerr := terminal.IsColor()
	if cfg.LogColor && cerr != nil {
		cfg.LogColor = false
	}
	if len(cfg.LogOutputs) == 0 {
		return errors.New("LogOutputs is not empty")
	}

	if cfg.ConfigPath == "" {
		rootDir, err := os.Getwd()
		if err != nil {
			rootDir = filepath.Join(os.TempDir(), cfg.Name)
			if err := os.MkdirAll(rootDir, 0700); err != nil {
				return err
			}
		}
		cfg.ConfigPath = filepath.Join(rootDir, cfg.Name+".yaml")
		var p string
		p, err = filepath.Abs(cfg.ConfigPath)
		if err != nil {
			panic(err)
		}
		cfg.ConfigPath = p
	}
	if err := os.MkdirAll(filepath.Dir(cfg.ConfigPath), 0700); err != nil {
		return err
	}
	if err := fileutil.IsDirWriteable(filepath.Dir(cfg.ConfigPath)); err != nil {
		return err
	}

	if len(cfg.LogOutputs) == 1 && (cfg.LogOutputs[0] == "stderr" || cfg.LogOutputs[0] == "stdout") {
		cfg.LogOutputs = append(cfg.LogOutputs, cfg.ConfigPath+".log")
	}
	if cfg.ASGsLogsDir == "" {
		cfg.ASGsLogsDir = filepath.Join(filepath.Dir(cfg.ConfigPath), cfg.Name+"-logs-remote")
	}

	if cfg.RemoteAccessCommandsOutputPath == "" {
		cfg.RemoteAccessCommandsOutputPath = strings.ReplaceAll(cfg.ConfigPath, ".yaml", "") + ".ssh.sh"
	}
	if filepath.Ext(cfg.RemoteAccessCommandsOutputPath) != ".sh" {
		cfg.RemoteAccessCommandsOutputPath = cfg.RemoteAccessCommandsOutputPath + ".sh"
	}
	if err := fileutil.IsDirWriteable(filepath.Dir(cfg.RemoteAccessCommandsOutputPath)); err != nil {
		return err
	}

	switch cfg.S3BucketCreate {
	case true: // need create one, or already created
		if cfg.S3BucketName == "" {
			cfg.S3BucketName = cfg.Name + "-s3-bucket"
		}
		if cfg.S3BucketLifecycleExpirationDays > 0 && cfg.S3BucketLifecycleExpirationDays < 3 {
			cfg.S3BucketLifecycleExpirationDays = 3
		}
	case false: // use existing one
	}

	switch cfg.RoleCreate {
	case true: // need create one, or already created
		if cfg.RoleName == "" {
			cfg.RoleName = cfg.Name + "-role"
		}
		if cfg.RoleARN != "" {
			// just ignore...
			// could be populated from previous run
			// do not error, so long as RoleCreate false, role won't be deleted
		}
		if len(cfg.RoleServicePrincipals) > 0 {
			/*
				create node group request failed (InvalidParameterException: Following required service principals [ec2.amazonaws.com] were not found in the trust relationships of nodeRole arn:aws:iam::...:role/test-mng-role
				{
				  ClusterName: "test",
				  Message_: "Following required service principals [ec2.amazonaws.com] were not found in the trust relationships of nodeRole arn:aws:iam::...:role/test-mng-role",
				  NodegroupName: "test-mng-cpu"
				})
			*/
			found := false
			for _, pv := range cfg.RoleServicePrincipals {
				if pv == "ec2.amazonaws.com" { // TODO: support China regions ec2.amazonaws.com.cn or eks.amazonaws.com.cn
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("RoleServicePrincipals %q must include 'ec2.amazonaws.com'", cfg.RoleServicePrincipals)
			}
		}

	case false: // use existing one
		if cfg.RoleARN == "" {
			return fmt.Errorf("Parameters.RoleCreate false; expect non-empty RoleARN but got %q", cfg.RoleARN)
		}
		if cfg.RoleName == "" {
			cfg.RoleName = getNameFromARN(cfg.RoleARN)
		}
		if len(cfg.RoleManagedPolicyARNs) > 0 {
			return fmt.Errorf("Parameters.RoleCreate false; expect empty RoleManagedPolicyARNs but got %q", cfg.RoleManagedPolicyARNs)
		}
		if len(cfg.RoleServicePrincipals) > 0 {
			return fmt.Errorf("Parameters.RoleCreate false; expect empty RoleServicePrincipals but got %q", cfg.RoleServicePrincipals)
		}
	}

	switch cfg.VPCCreate {
	case true: // need create one, or already created
		if cfg.VPCID != "" {
			// just ignore...
			// could be populated from previous run
			// do not error, so long as VPCCreate false, VPC won't be deleted
		}
	case false: // use existing one
		if cfg.VPCID == "" {
			return fmt.Errorf("Parameters.RoleCreate false; expect non-empty VPCID but got %q", cfg.VPCID)
		}
	}

	switch {
	case cfg.VPCCIDR != "":
		switch {
		case cfg.PublicSubnetCIDR1 == "":
			return fmt.Errorf("empty Parameters.PublicSubnetCIDR1 when VPCCIDR is %q", cfg.VPCCIDR)
		case cfg.PublicSubnetCIDR2 == "":
			return fmt.Errorf("empty Parameters.PublicSubnetCIDR2 when VPCCIDR is %q", cfg.VPCCIDR)
		case cfg.PublicSubnetCIDR3 == "":
			return fmt.Errorf("empty Parameters.PublicSubnetCIDR3 when VPCCIDR is %q", cfg.VPCCIDR)
		case cfg.PrivateSubnetCIDR1 == "":
			return fmt.Errorf("empty Parameters.PrivateSubnetCIDR1 when VPCCIDR is %q", cfg.VPCCIDR)
		case cfg.PrivateSubnetCIDR2 == "":
			return fmt.Errorf("empty Parameters.PrivateSubnetCIDR2 when VPCCIDR is %q", cfg.VPCCIDR)
		}

	case cfg.VPCCIDR == "":
		switch {
		case cfg.PublicSubnetCIDR1 != "":
			return fmt.Errorf("non-empty Parameters.PublicSubnetCIDR1 %q when VPCCIDR is empty", cfg.PublicSubnetCIDR1)
		case cfg.PublicSubnetCIDR2 != "":
			return fmt.Errorf("non-empty Parameters.PublicSubnetCIDR2 %q when VPCCIDR is empty", cfg.PublicSubnetCIDR2)
		case cfg.PublicSubnetCIDR3 != "":
			return fmt.Errorf("non-empty Parameters.PublicSubnetCIDR3 %q when VPCCIDR is empty", cfg.PublicSubnetCIDR3)
		case cfg.PrivateSubnetCIDR1 != "":
			return fmt.Errorf("non-empty Parameters.PrivateSubnetCIDR1 %q when VPCCIDR is empty", cfg.PrivateSubnetCIDR1)
		case cfg.PrivateSubnetCIDR2 != "":
			return fmt.Errorf("non-empty Parameters.PrivateSubnetCIDR2 %q when VPCCIDR is empty", cfg.PrivateSubnetCIDR2)
		}
	}

	switch cfg.RemoteAccessKeyCreate {
	case true: // need create one, or already created
		if cfg.RemoteAccessKeyName == "" {
			cfg.RemoteAccessKeyName = cfg.Name + "-key"
		}
		if cfg.RemoteAccessPrivateKeyPath == "" {
			cfg.RemoteAccessPrivateKeyPath = filepath.Join(os.TempDir(), randutil.String(10)+".insecure.key")
		}

	case false: // use existing one
		if cfg.RemoteAccessKeyName == "" {
			return fmt.Errorf("RemoteAccessKeyCreate false; expect non-empty RemoteAccessKeyName but got %q", cfg.RemoteAccessKeyName)
		}
		if cfg.RemoteAccessPrivateKeyPath == "" {
			return fmt.Errorf("RemoteAccessKeyCreate false; expect non-empty RemoteAccessPrivateKeyPath but got %q", cfg.RemoteAccessPrivateKeyPath)
		}
		if !fileutil.Exist(cfg.RemoteAccessPrivateKeyPath) {
			return fmt.Errorf("RemoteAccessPrivateKeyPath %q does not exist", cfg.RemoteAccessPrivateKeyPath)
		}
	}
	if err := fileutil.IsDirWriteable(filepath.Dir(cfg.RemoteAccessPrivateKeyPath)); err != nil {
		return err
	}

	return nil
}

func (cfg *Config) validateASGs() error {
	n := len(cfg.ASGs)
	if n == 0 {
		return errors.New("empty ASGs")
	}
	if n > ASGsMaxLimit {
		return fmt.Errorf("ASGs %d exceeds maximum number of ASGs which is %d", n, ASGsMaxLimit)
	}
	names, processed := make(map[string]struct{}), make(map[string]ASG)
	for k, v := range cfg.ASGs {
		k = strings.ReplaceAll(k, "GetRef.Name", cfg.Name)
		v.Name = strings.ReplaceAll(v.Name, "GetRef.Name", cfg.Name)

		if v.Name == "" {
			return fmt.Errorf("ASGs[%q].Name is empty", k)
		}
		if k != v.Name {
			return fmt.Errorf("ASGs[%q].Name has different Name field %q", k, v.Name)
		}
		_, ok := names[v.Name]
		if !ok {
			names[v.Name] = struct{}{}
		} else {
			return fmt.Errorf("ASGs[%q].Name %q is redundant", k, v.Name)
		}

		if len(v.InstanceTypes) > 4 {
			return fmt.Errorf("too many InstaceTypes[%q]", v.InstanceTypes)
		}
		if v.VolumeSize == 0 {
			v.VolumeSize = DefaultNodeVolumeSize
		}
		if v.RemoteAccessUserName == "" {
			v.RemoteAccessUserName = "ec2-user"
		}

		if v.ImageID == "" && v.ImageIDSSMParameter == "" {
			return fmt.Errorf("%q both ImageID and ImageIDSSMParameter are empty", v.Name)
		}

		switch v.AMIType {
		case AMITypeBottleRocketCPU:
			if v.RemoteAccessUserName != "ec2-user" {
				return fmt.Errorf("AMIType %q but unexpected RemoteAccessUserName %q", v.AMIType, v.RemoteAccessUserName)
			}
		case AMITypeAL2X8664:
			if v.RemoteAccessUserName != "ec2-user" {
				return fmt.Errorf("AMIType %q but unexpected RemoteAccessUserName %q", v.AMIType, v.RemoteAccessUserName)
			}
		case AMITypeAL2X8664GPU:
			if v.RemoteAccessUserName != "ec2-user" {
				return fmt.Errorf("AMIType %q but unexpected RemoteAccessUserName %q", v.AMIType, v.RemoteAccessUserName)
			}
		default:
			return fmt.Errorf("unknown ASGs[%q].AMIType %q", k, v.AMIType)
		}

		switch v.AMIType {
		case AMITypeBottleRocketCPU:
			if len(v.InstanceTypes) == 0 {
				v.InstanceTypes = []string{DefaultNodeInstanceTypeCPU}
			}
		case AMITypeAL2X8664:
			if len(v.InstanceTypes) == 0 {
				v.InstanceTypes = []string{DefaultNodeInstanceTypeCPU}
			}
		case AMITypeAL2X8664GPU:
			if len(v.InstanceTypes) == 0 {
				v.InstanceTypes = []string{DefaultNodeInstanceTypeGPU}
			}
		default:
			return fmt.Errorf("unknown ASGs[%q].AMIType %q", k, v.AMIType)
		}

		if v.ASGMinSize > v.ASGMaxSize {
			return fmt.Errorf("ASGs[%q].ASGMinSize %d > ASGMaxSize %d", k, v.ASGMinSize, v.ASGMaxSize)
		}
		if v.ASGDesiredCapacity > v.ASGMaxSize {
			return fmt.Errorf("ASGs[%q].ASGDesiredCapacity %d > ASGMaxSize %d", k, v.ASGDesiredCapacity, v.ASGMaxSize)
		}
		if v.ASGMaxSize > ASGMaxLimit {
			return fmt.Errorf("ASGs[%q].ASGMaxSize %d > ASGMaxLimit %d", k, v.ASGMaxSize, ASGMaxLimit)
		}
		if v.ASGDesiredCapacity > ASGMaxLimit {
			return fmt.Errorf("ASGs[%q].ASGDesiredCapacity %d > ASGMaxLimit %d", k, v.ASGDesiredCapacity, ASGMaxLimit)
		}

		switch v.SSMDocumentCreate {
		case true: // need create one, or already created
			if v.SSMDocumentCFNStackName == "" {
				v.SSMDocumentCFNStackName = v.Name + "-ssm-document"
			}
			if v.SSMDocumentName == "" {
				v.SSMDocumentName = v.Name + "SSMDocument"
			}
			v.SSMDocumentCFNStackName = strings.ReplaceAll(v.SSMDocumentCFNStackName, "GetRef.Name", cfg.Name)
			v.SSMDocumentName = strings.ReplaceAll(v.SSMDocumentName, "GetRef.Name", cfg.Name)
			v.SSMDocumentName = ssmDocNameRegex.ReplaceAllString(v.SSMDocumentName, "")
			if v.SSMDocumentExecutionTimeoutSeconds == 0 {
				v.SSMDocumentExecutionTimeoutSeconds = 3600
			}

		case false: // use existing one, or don't run any SSM
		}

		processed[k] = v
	}

	cfg.ASGs = processed
	return nil
}

// only letters and numbers
var ssmDocNameRegex = regexp.MustCompile("[^a-zA-Z0-9]+")

// get "role-eks" from "arn:aws:iam::123:role/role-eks"
func getNameFromARN(arn string) string {
	if ss := strings.Split(arn, "/"); len(ss) > 0 {
		arn = ss[len(ss)-1]
	}
	return arn
}

func getTS() string {
	now := time.Now()
	return fmt.Sprintf(
		"%04d%02d%02d%02d%02d",
		now.Year(),
		int(now.Month()),
		now.Day(),
		now.Hour(),
		now.Second(),
	)
}
