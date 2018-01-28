/*
Copyright 2016 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// TODO: We should replace most of this code with a fast-install manifest
// This would also allow more customization, and get rid of half of this code
// BUT... there's a circular dependency in the PRs here... :-)

package imagebuilder

import (
	"fmt"
	"time"

	"golang.org/x/crypto/ssh"

	"crypto/md5"
	"encoding/hex"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/golang/glog"
)

const tagRoleKey = "k8s.io/role/imagebuilder"

// AWSInstance manages an AWS instance, used for building an image
type AWSInstance struct {
	instanceID string
	cloud      *AWSCloud
	instance   *ec2.Instance
}

// Shutdown terminates the running instance
func (i *AWSInstance) Shutdown() error {
	glog.Infof("Terminating instance %q", i.instanceID)
	return i.cloud.TerminateInstance(i.instanceID)
}

// DialSSH establishes an SSH client connection to the instance
func (i *AWSInstance) DialSSH(config *ssh.ClientConfig) (*ssh.Client, error) {
	publicIP, err := i.WaitPublicIP()
	if err != nil {
		return nil, err
	}

	for {
		// TODO: Timeout, check error code
		sshClient, err := ssh.Dial("tcp", publicIP+":22", config)
		if err != nil {
			glog.Warningf("error connecting to SSH on server %q: %v", publicIP, err)
			time.Sleep(5 * time.Second)
			continue
			//	return nil, fmt.Errorf("error connecting to SSH on server %q", publicIP)
		}

		return sshClient, nil
	}
}

// WaitPublicIP waits for the instance to get a public IP, returning it
func (i *AWSInstance) WaitPublicIP() (string, error) {
	// TODO: Timeout
	for {
		instance, err := i.cloud.describeInstance(i.instanceID)
		if err != nil {
			return "", err
		}
		publicIP := aws.StringValue(instance.PublicIpAddress)
		if publicIP != "" {
			glog.Infof("Instance public IP is %q", publicIP)
			return publicIP, nil
		}
		glog.V(2).Infof("Sleeping before requerying instance for public IP: %q", i.instanceID)
		time.Sleep(5 * time.Second)
	}
}

// AWSCloud is a helper type for talking to an AWS acccount
type AWSCloud struct {
	config *AWSConfig

	ec2 *ec2.EC2
}

var _ Cloud = &AWSCloud{}

func NewAWSCloud(ec2 *ec2.EC2, config *AWSConfig) *AWSCloud {
	return &AWSCloud{
		ec2:    ec2,
		config: config,
	}
}

func (a *AWSCloud) GetExtraEnv() (map[string]string, error) {
	credentials := a.ec2.Config.Credentials
	if credentials == nil {
		return nil, fmt.Errorf("unable to determine EC2 credentials")
	}

	creds, err := credentials.Get()
	if err != nil {
		return nil, fmt.Errorf("error fetching EC2 credentials: %v", err)
	}

	env := make(map[string]string)
	env["AWS_ACCESS_KEY"] = creds.AccessKeyID
	env["AWS_SECRET_KEY"] = creds.SecretAccessKey

	return env, nil
}

func (a *AWSCloud) describeInstance(instanceID string) (*ec2.Instance, error) {
	request := &ec2.DescribeInstancesInput{}
	request.InstanceIds = []*string{&instanceID}

	glog.V(2).Infof("AWS DescribeInstances InstanceId=%q", instanceID)
	response, err := a.ec2.DescribeInstances(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeInstances call: %v", err)
	}

	for _, reservation := range response.Reservations {
		for _, instance := range reservation.Instances {
			if aws.StringValue(instance.InstanceId) != instanceID {
				panic("Unexpected InstanceId found")
			}

			return instance, err
		}
	}
	return nil, nil
}

// TerminateInstance terminates the specified instance
func (a *AWSCloud) TerminateInstance(instanceID string) error {
	request := &ec2.TerminateInstancesInput{}
	request.InstanceIds = []*string{&instanceID}

	glog.V(2).Infof("AWS TerminateInstances instanceID=%q", instanceID)
	_, err := a.ec2.TerminateInstances(request)
	return err
}

// GetInstance returns the AWS instance matching our tags, or nil if not found
func (a *AWSCloud) GetInstance() (Instance, error) {
	request := &ec2.DescribeInstancesInput{}
	request.Filters = []*ec2.Filter{
		{
			Name:   aws.String("tag-key"),
			Values: aws.StringSlice([]string{tagRoleKey}),
		},
	}

	glog.V(2).Infof("AWS DescribeInstances Filter:tag-key=%s", tagRoleKey)
	response, err := a.ec2.DescribeInstances(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeInstances call: %v", err)
	}

	for _, reservation := range response.Reservations {
		for _, instance := range reservation.Instances {
			instanceID := aws.StringValue(instance.InstanceId)
			if instanceID == "" {
				panic("Found instance with empty instance ID")
			}

			glog.Infof("Found existing instance: %q", instanceID)
			return &AWSInstance{
				cloud:      a,
				instance:   instance,
				instanceID: instanceID,
			}, nil
		}
	}

	return nil, nil
}

// findSubnet returns a subnet tagged with our role tag, if one exists
func (c *AWSCloud) findSubnet() (*ec2.Subnet, error) {
	request := &ec2.DescribeSubnetsInput{}
	request.Filters = []*ec2.Filter{
		{
			Name:   aws.String("tag-key"),
			Values: aws.StringSlice([]string{tagRoleKey}),
		},
	}

	glog.V(2).Infof("AWS DescribeSubnets Filter:tag-key=%s", tagRoleKey)
	response, err := c.ec2.DescribeSubnets(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeSubnets call: %v", err)
	}

	for _, subnet := range response.Subnets {
		return subnet, nil
	}

	return nil, nil
}

// findSecurityGroup returns a security group tagged with our role tag, if one exists
func (c *AWSCloud) findSecurityGroup(vpcID string) (*ec2.SecurityGroup, error) {
	request := &ec2.DescribeSecurityGroupsInput{}
	request.Filters = []*ec2.Filter{
		{
			Name:   aws.String("tag-key"),
			Values: aws.StringSlice([]string{tagRoleKey}),
		},
		{
			Name:   aws.String("vpc-id"),
			Values: aws.StringSlice([]string{vpcID}),
		},
	}

	glog.V(2).Infof("AWS DescribeSecurityGroups Filter:tag-key=%s", tagRoleKey)
	response, err := c.ec2.DescribeSecurityGroups(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeSecurityGroups call: %v", err)
	}

	for _, sg := range response.SecurityGroups {
		return sg, nil
	}

	return nil, nil
}

// describeSubnet returns a subnet with the specified id, if it exists
func (c *AWSCloud) describeSubnet(subnetID string) (*ec2.Subnet, error) {
	request := &ec2.DescribeSubnetsInput{}
	request.SubnetIds = []*string{&subnetID}

	glog.V(2).Infof("AWS DescribeSubnetsInput ID:%q", subnetID)
	response, err := c.ec2.DescribeSubnets(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeSubnets call: %v", err)
	}

	for _, subnet := range response.Subnets {
		return subnet, nil
	}

	return nil, nil
}

// TagResource adds AWS tags to the specified resource
func (a *AWSCloud) TagResource(resourceId string, tags ...*ec2.Tag) error {
	request := &ec2.CreateTagsInput{}
	request.Resources = aws.StringSlice([]string{resourceId})
	request.Tags = tags

	glog.V(2).Infof("AWS CreateTags Resource=%q", resourceId)
	_, err := a.ec2.CreateTags(request)
	if err != nil {
		return fmt.Errorf("error making AWS CreateTag call: %v", err)
	}

	return err
}

func (c *AWSCloud) findSSHKey(name string) (*ec2.KeyPairInfo, error) {
	request := &ec2.DescribeKeyPairsInput{
		KeyNames: []*string{&name},
	}

	response, err := c.ec2.DescribeKeyPairs(request)
	if awsErr, ok := err.(awserr.Error); ok {
		if awsErr.Code() == "InvalidKeyPair.NotFound" {
			return nil, nil
		}
	}
	if err != nil {
		return nil, fmt.Errorf("error listing AWS KeyPairs: %v", err)
	}

	if response == nil || len(response.KeyPairs) == 0 {
		return nil, nil
	}

	if len(response.KeyPairs) != 1 {
		return nil, fmt.Errorf("Found multiple AWS KeyPairs with Name %q", name)
	}

	k := response.KeyPairs[0]

	return k, nil
}
func (c *AWSCloud) ensureSSHKey() (string, error) {
	publicKey, err := ReadFile(c.config.SSHPublicKey)
	if err != nil {
		return "", err
	}

	// TODO: Use real OpenSSH or AWS fingerprint?
	hashBytes := md5.Sum([]byte(publicKey))
	hash := hex.EncodeToString(hashBytes[:])

	name := "imagebuilder-" + hash

	key, err := c.findSSHKey(name)
	if err != nil {
		return "", err
	}

	if key != nil {
		return *key.KeyName, nil
	}

	glog.V(2).Infof("Creating AWS KeyPair with Name:%q", name)

	request := &ec2.ImportKeyPairInput{}
	request.KeyName = &name
	request.PublicKeyMaterial = []byte(publicKey)

	response, err := c.ec2.ImportKeyPair(request)
	if err != nil {
		return "", fmt.Errorf("error creating AWS KeyPair: %v", err)
	}

	return *response.KeyName, nil
}

// CreateInstance creates an instance for building an image instance
func (c *AWSCloud) CreateInstance() (Instance, error) {
	var err error
	sshKeyName := c.config.SSHKeyName
	if sshKeyName == "" {
		sshKeyName, err = c.ensureSSHKey()
		if err != nil {
			return nil, err
		}
	}

	subnetID := c.config.SubnetID
	if subnetID == "" {
		subnet, err := c.findSubnet()
		if err != nil {
			return nil, err
		}
		if subnet != nil {
			subnetID = aws.StringValue(subnet.SubnetId)
		}
		if subnetID == "" {
			return nil, fmt.Errorf("SubnetID must be specified, or a subnet must be tagged with %q", tagRoleKey)
		}
	}

	subnet, err := c.describeSubnet(subnetID)
	if err != nil {
		return nil, err
	}
	if subnet == nil {
		return nil, fmt.Errorf("could not find subnet %q", subnetID)
	}

	if c.config.ImageID == "" {
		return nil, fmt.Errorf("ImageID must be specified")
	}

	if c.config.InstanceType == "" {
		return nil, fmt.Errorf("InstanceType must be specified")
	}

	securityGroupID := c.config.SecurityGroupID
	if securityGroupID == "" {
		vpcID := *subnet.VpcId
		securityGroup, err := c.findSecurityGroup(vpcID)
		if err != nil {
			return nil, err
		}
		if securityGroup != nil {
			securityGroupID = aws.StringValue(securityGroup.GroupId)
		}
		if securityGroupID == "" {
			return nil, fmt.Errorf("SecurityGroupID must be specified, or a security group for VPC %q must be tagged with %q", vpcID, tagRoleKey)
		}
	}

	request := &ec2.RunInstancesInput{}
	request.ImageId = aws.String(c.config.ImageID)
	request.KeyName = aws.String(sshKeyName)
	request.InstanceType = aws.String(c.config.InstanceType)
	request.NetworkInterfaces = []*ec2.InstanceNetworkInterfaceSpecification{
		{
			DeviceIndex:              aws.Int64(0),
			AssociatePublicIpAddress: aws.Bool(true),
			SubnetId:                 aws.String(subnetID),
			Groups:                   aws.StringSlice([]string{securityGroupID}),
		},
	}
	request.MaxCount = aws.Int64(1)
	request.MinCount = aws.Int64(1)

	glog.V(2).Infof("AWS RunInstances InstanceType=%q ImageId=%q KeyName=%q", c.config.InstanceType, c.config.ImageID, sshKeyName)
	response, err := c.ec2.RunInstances(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS RunInstances call: %v", err)
	}

	for _, instance := range response.Instances {
		instanceID := aws.StringValue(instance.InstanceId)
		if instanceID == "" {
			return nil, fmt.Errorf("AWS RunInstances call returned empty InstanceId")
		}
		err := c.TagResource(instanceID, &ec2.Tag{
			Key: aws.String(tagRoleKey), Value: aws.String("'"),
		})
		if err != nil {
			glog.Warningf("Tagging instance %q failed; will terminate to prevent leaking", instanceID)
			e2 := c.TerminateInstance(instanceID)
			if e2 != nil {
				glog.Warningf("error terminating instance %q, will leak instance", instanceID)
			}
			return nil, err
		}

		return &AWSInstance{
			cloud:      c,
			instance:   instance,
			instanceID: instanceID,
		}, nil
	}
	return nil, fmt.Errorf("instance was not returned by AWS RunInstances")
}

// FindImage finds a registered image, matching by the name tag
func (a *AWSCloud) FindImage(imageName string) (Image, error) {
	image, err := findAWSImage(a.ec2, imageName)
	if err != nil {
		return nil, err
	}

	if image == nil {
		return nil, nil
	}

	imageID := aws.StringValue(image.ImageId)
	if imageID == "" {
		return nil, fmt.Errorf("found image with empty ImageId: %q", imageName)
	}

	return &AWSImage{
		ec2:     a.ec2,
		region:  a.config.Region,
		image:   image,
		imageID: imageID,
	}, nil
}

func findAWSImage(client *ec2.EC2, imageName string) (*ec2.Image, error) {
	request := &ec2.DescribeImagesInput{}
	request.Filters = []*ec2.Filter{
		{
			Name:   aws.String("name"),
			Values: aws.StringSlice([]string{imageName}),
		},
	}
	request.Owners = aws.StringSlice([]string{"self"})

	glog.V(2).Infof("AWS DescribeImages Filter:Name=%q, Owner=self", imageName)
	response, err := client.DescribeImages(request)
	if err != nil {
		return nil, fmt.Errorf("error making AWS DescribeImages call: %v", err)
	}

	if len(response.Images) == 0 {
		return nil, nil
	}

	if len(response.Images) != 1 {
		// Image names are unique per user...
		return nil, fmt.Errorf("found multiple matching images for name: %q", imageName)
	}

	image := response.Images[0]
	return image, nil
}

// AWSImage represents an AMI on AWS
type AWSImage struct {
	ec2    *ec2.EC2
	region string
	//cloud   *AWSCloud
	image   *ec2.Image
	imageID string
}

// ID returns the AWS identifier for the image
func (i *AWSImage) ID() string {
	return i.imageID
}

// String returns a string representation of the image
func (i *AWSImage) String() string {
	return "AWSImage[id=" + i.imageID + "]"
}

// EnsurePublic makes the image accessible outside the current account
func (i *AWSImage) EnsurePublic() error {
	return i.ensurePublic()
}

func (i *AWSImage) waitStatusAvailable() error {
	imageID := i.imageID

	for {
		// TODO: Timeout
		request := &ec2.DescribeImagesInput{}
		request.ImageIds = aws.StringSlice([]string{imageID})

		glog.V(2).Infof("AWS DescribeImages ImageId=%q", imageID)
		response, err := i.ec2.DescribeImages(request)
		if err != nil {
			return fmt.Errorf("error making AWS DescribeImages call: %v", err)
		}

		if len(response.Images) == 0 {
			return fmt.Errorf("image not found %q", imageID)
		}

		if len(response.Images) != 1 {
			return fmt.Errorf("multiple imags found with ID %q", imageID)
		}

		image := response.Images[0]

		state := aws.StringValue(image.State)
		glog.V(2).Infof("image state %q", state)
		if state == "available" {
			return nil
		}
		glog.Infof("Image not yet available (%s); waiting", imageID)
		time.Sleep(10 * time.Second)
	}
}

func (i *AWSImage) ensurePublic() error {
	err := i.waitStatusAvailable()
	if err != nil {
		return err
	}

	// This is idempotent, so just always do it
	request := &ec2.ModifyImageAttributeInput{}
	request.ImageId = aws.String(i.imageID)
	request.LaunchPermission = &ec2.LaunchPermissionModifications{
		Add: []*ec2.LaunchPermission{
			{Group: aws.String("all")},
		},
	}

	glog.V(2).Infof("AWS ModifyImageAttribute Image=%q, LaunchPermission All", i.image)
	_, err = i.ec2.ModifyImageAttribute(request)
	if err != nil {
		return fmt.Errorf("error making image public %q: %v", i.imageID, err)
	}

	return err
}

// ReplicateImage copies the image to all accessable AWS regions
func (i *AWSImage) ReplicateImage(makePublic bool) (map[string]Image, error) {
	imagesByRegion := make(map[string]Image)

	glog.V(2).Infof("AWS DescribeRegions")
	request := &ec2.DescribeRegionsInput{}
	response, err := i.ec2.DescribeRegions(request)
	if err != nil {
		return nil, fmt.Errorf("error listing ec2 regions: %v", err)

	}
	imagesByRegion[i.region] = i

	for _, region := range response.Regions {
		regionName := aws.StringValue(region.RegionName)
		if imagesByRegion[regionName] != nil {
			continue
		}

		imageID, err := i.copyImageToRegion(regionName)
		if err != nil {
			return nil, fmt.Errorf("error copying image to region %q: %v", regionName, err)
		}
		targetEC2 := ec2.New(session.New(), &aws.Config{Region: &regionName})
		imagesByRegion[regionName] = &AWSImage{
			ec2:     targetEC2,
			region:  regionName,
			imageID: imageID,
		}
	}

	if makePublic {
		for regionName, image := range imagesByRegion {
			err := image.EnsurePublic()
			if err != nil {
				return nil, fmt.Errorf("error making image public in region %q: %v", regionName, err)
			}
		}
	}

	return imagesByRegion, nil
}

func (i *AWSImage) copyImageToRegion(regionName string) (string, error) {
	targetEC2 := ec2.New(session.New(), &aws.Config{Region: &regionName})

	imageName := aws.StringValue(i.image.Name)
	description := aws.StringValue(i.image.Description)

	destImage, err := findAWSImage(targetEC2, imageName)
	if err != nil {
		return "", err
	}

	var imageID string

	// We've already copied the image
	if destImage != nil {
		imageID = aws.StringValue(destImage.ImageId)
	} else {
		token := imageName + "-" + regionName

		request := &ec2.CopyImageInput{
			ClientToken:   aws.String(token),
			Description:   aws.String(description),
			Name:          aws.String(imageName),
			SourceImageId: aws.String(i.imageID),
			SourceRegion:  aws.String(i.region),
		}
		glog.V(2).Infof("AWS CopyImage Image=%q, Region=%q", i.imageID, regionName)
		response, err := targetEC2.CopyImage(request)
		if err != nil {
			return "", fmt.Errorf("error copying image to region %q: %v", regionName, err)
		}

		imageID = aws.StringValue(response.ImageId)
	}

	return imageID, nil
}
