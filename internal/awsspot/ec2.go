package awsspot

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"
	"net/netip"
	"regexp"
	"sluicework/internal/spot"
	"strconv"
)

const tagBackend = "sluicework:backend"
const tagRequest = "sluicework:request"
const tagExpires = "sluicework:expires"
const tagToken = "sluicework:token"

type EC2API interface {
	RunInstances(context.Context, *ec2.RunInstancesInput, ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error)
	DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error)
	TerminateInstances(context.Context, *ec2.TerminateInstancesInput, ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error)
	DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
	DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
	DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error)
}
type EC2Cloud struct {
	Client               EC2API
	BackendID, AccountID string
}

func apiCode(e error) string {
	var api smithy.APIError
	if errors.As(e, &api) {
		return api.ErrorCode()
	}
	return "UnconfirmedRequest"
}
func (c *EC2Cloud) ValidateImage(ctx context.Context, image, root string) error {
	out, e := c.Client.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: []string{image}})
	if e != nil {
		return errors.New("cannot validate the Spot runner image")
	}
	if len(out.Images) != 1 {
		return errors.New("Spot runner image is unavailable")
	}
	v := out.Images[0]
	if v.Architecture != types.ArchitectureValuesArm64 || v.State != types.ImageStateAvailable || aws.ToString(v.RootDeviceName) != root || v.RootDeviceType != types.DeviceTypeEbs || (aws.ToString(v.OwnerId) != "099720109477" && aws.ToString(v.OwnerId) != c.AccountID) {
		return errors.New("use an available canonical Ubuntu or account-owned ARM64 EBS image")
	}
	return nil
}

// ValidateNetwork fails closed unless the runner security group is still
// outbound-only (TCP 80/443 to address ranges) and shares the subnet's VPC.
// Security groups are mutable outside the reviewed template, and every runner
// receives a public IPv4 address, so this is checked before every launch.
func (c *EC2Cloud) ValidateNetwork(ctx context.Context, subnet, group string) error {
	groups, e := c.Client.DescribeSecurityGroups(ctx, &ec2.DescribeSecurityGroupsInput{GroupIds: []string{group}})
	if e != nil {
		return errors.New("cannot validate the Spot runner security group")
	}
	if groups == nil || len(groups.SecurityGroups) != 1 || aws.ToString(groups.SecurityGroups[0].GroupId) != group {
		return errors.New("Spot runner security group is unavailable")
	}
	g := groups.SecurityGroups[0]
	if len(g.IpPermissions) != 0 {
		return errors.New("Spot runner security group must not allow inbound traffic")
	}
	if len(g.IpPermissionsEgress) == 0 {
		return errors.New("Spot runner security group allows no outbound HTTPS")
	}
	https := false
	for _, p := range g.IpPermissionsEgress {
		port := aws.ToInt32(p.FromPort)
		if aws.ToString(p.IpProtocol) != "tcp" || port != aws.ToInt32(p.ToPort) || (port != 80 && port != 443) || len(p.UserIdGroupPairs) != 0 || len(p.PrefixListIds) != 0 {
			return errors.New("Spot runner security group may only allow outbound TCP 80 and 443 to address ranges")
		}
		if len(p.IpRanges)+len(p.Ipv6Ranges) == 0 {
			return errors.New("Spot runner egress must name an address range")
		}
		for _, r := range p.IpRanges {
			prefix, e := netip.ParsePrefix(aws.ToString(r.CidrIp))
			if e != nil || !prefix.Addr().Is4() {
				return errors.New("Spot runner egress has an invalid IPv4 range")
			}
		}
		for _, r := range p.Ipv6Ranges {
			prefix, e := netip.ParsePrefix(aws.ToString(r.CidrIpv6))
			if e != nil || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
				return errors.New("Spot runner egress has an invalid IPv6 range")
			}
		}
		https = https || port == 443
	}
	if !https {
		return errors.New("Spot runner security group allows no outbound HTTPS")
	}
	subnets, e := c.Client.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{SubnetIds: []string{subnet}})
	if e != nil {
		return errors.New("cannot validate the Spot runner subnet")
	}
	if subnets == nil || len(subnets.Subnets) != 1 || aws.ToString(subnets.Subnets[0].SubnetId) != subnet || aws.ToString(subnets.Subnets[0].VpcId) == "" || aws.ToString(subnets.Subnets[0].VpcId) != aws.ToString(g.VpcId) {
		return errors.New("Spot runner subnet and security group must share one VPC")
	}
	return nil
}
func (c *EC2Cloud) Launch(ctx context.Context, s spot.LaunchSpec) (string, error) {
	if s.BackendID != c.BackendID {
		return "", errors.New("Spot launch selected another backend")
	}
	script, e := RenderBootstrap(s)
	if e != nil {
		return "", fmt.Errorf("%w: invalid bootstrap", spot.ErrLaunchRejected)
	}
	if e = c.ValidateImage(ctx, s.ImageID, s.RootDeviceName); e != nil {
		return "", fmt.Errorf("%w: image validation failed", spot.ErrLaunchRejected)
	}
	if e = c.ValidateNetwork(ctx, s.SubnetID, s.SecurityGroupID); e != nil {
		return "", fmt.Errorf("%w: %v", spot.ErrLaunchRejected, e)
	}
	tags := []types.Tag{{Key: aws.String(tagBackend), Value: aws.String(c.BackendID)}, {Key: aws.String(tagRequest), Value: aws.String(s.Job.ID)}, {Key: aws.String(tagExpires), Value: aws.String(strconv.FormatInt(s.Expires.Unix(), 10))}, {Key: aws.String(tagToken), Value: aws.String(s.ClientToken)}}
	out, e := c.Client.RunInstances(ctx, &ec2.RunInstancesInput{
		ClientToken: aws.String(s.ClientToken), ImageId: aws.String(s.ImageID), InstanceType: types.InstanceType(s.Profile.InstanceType), MinCount: aws.Int32(1), MaxCount: aws.Int32(1),
		NetworkInterfaces:                 []types.InstanceNetworkInterfaceSpecification{{DeviceIndex: aws.Int32(0), SubnetId: aws.String(s.SubnetID), Groups: []string{s.SecurityGroupID}, AssociatePublicIpAddress: aws.Bool(true), DeleteOnTermination: aws.Bool(true)}},
		InstanceMarketOptions:             &types.InstanceMarketOptionsRequest{MarketType: types.MarketTypeSpot, SpotOptions: &types.SpotMarketOptions{SpotInstanceType: types.SpotInstanceTypeOneTime, InstanceInterruptionBehavior: types.InstanceInterruptionBehaviorTerminate, MaxPrice: aws.String(s.Profile.MaxPrice)}},
		InstanceInitiatedShutdownBehavior: types.ShutdownBehaviorTerminate,
		MetadataOptions:                   &types.InstanceMetadataOptionsRequest{HttpTokens: types.HttpTokensStateRequired, HttpPutResponseHopLimit: aws.Int32(1), InstanceMetadataTags: types.InstanceMetadataTagsStateDisabled},
		BlockDeviceMappings:               []types.BlockDeviceMapping{{DeviceName: aws.String(s.RootDeviceName), Ebs: &types.EbsBlockDevice{Encrypted: aws.Bool(true), DeleteOnTermination: aws.Bool(true), VolumeType: types.VolumeTypeGp3, VolumeSize: aws.Int32(int32(s.VolumeGiB))}}},
		TagSpecifications:                 []types.TagSpecification{{ResourceType: types.ResourceTypeInstance, Tags: tags}, {ResourceType: types.ResourceTypeVolume, Tags: tags}},
		UserData:                          aws.String(base64.StdEncoding.EncodeToString([]byte(script))),
	})
	if e != nil {
		code := apiCode(e)
		switch code {
		case "InsufficientInstanceCapacity", "MaxSpotInstanceCountExceeded", "VcpuLimitExceeded", "Unsupported", "SpotMaxPriceTooLow", "InvalidAMIID.NotFound", "InvalidAMIID.Unavailable":
			return "", fmt.Errorf("%w (%s)", spot.ErrLaunchRejected, code)
		}
		return "", fmt.Errorf("Spot instance creation not confirmed (%s)", code)
	}
	if len(out.Instances) != 1 || aws.ToString(out.Instances[0].InstanceId) == "" {
		return "", spot.ErrUncertain
	}
	return aws.ToString(out.Instances[0].InstanceId), nil
}
func (c *EC2Cloud) Inspect(ctx context.Context, id string) (spot.Instance, error) {
	if !regexp.MustCompile(`^i-[a-f0-9]{8,17}$`).MatchString(id) {
		return spot.Instance{}, errors.New("invalid Spot instance identity")
	}
	out, e := c.Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{InstanceIds: []string{id}})
	if e != nil {
		return spot.Instance{}, fmt.Errorf("Spot instance inspection not confirmed (%s)", apiCode(e))
	}
	var matches []types.Instance
	for _, r := range out.Reservations {
		for _, v := range r.Instances {
			if aws.ToString(v.InstanceId) == id {
				matches = append(matches, v)
			}
		}
	}
	if len(matches) != 1 {
		return spot.Instance{}, errors.New("Spot instance is not yet visible")
	}
	v := matches[0]
	owned := false
	for _, tag := range v.Tags {
		if aws.ToString(tag.Key) == tagBackend && aws.ToString(tag.Value) == c.BackendID {
			owned = true
		}
	}
	state := "unknown"
	if v.State != nil {
		state = string(v.State.Name)
	}
	reason := ""
	if v.StateReason != nil {
		reason = aws.ToString(v.StateReason.Code)
	}
	return spot.Instance{ID: id, State: state, Reason: reason, Owned: owned}, nil
}
func (c *EC2Cloud) Terminate(ctx context.Context, id string) error {
	v, e := c.Inspect(ctx, id)
	if e != nil {
		return e
	}
	if !v.Owned {
		return errors.New("refusing to terminate an instance outside this Spot fleet")
	}
	if v.State == "terminated" {
		return nil
	}
	_, e = c.Client.TerminateInstances(ctx, &ec2.TerminateInstancesInput{InstanceIds: []string{id}})
	if e != nil {
		return fmt.Errorf("Spot termination not confirmed (%s)", apiCode(e))
	}
	return nil
}
func Interrupted(reason string) bool {
	return reason == "Server.SpotInstanceShutdown" || reason == "Server.SpotInstanceTermination"
}

var _ spot.Cloud = (*EC2Cloud)(nil)

// Inventory includes terminated instances while AWS retains them; absence alone
// is never proof that an uncertain RunInstances call did not create a machine.
func (c *EC2Cloud) Inventory(ctx context.Context) ([]spot.ManagedInstance, error) {
	var token *string
	out := []spot.ManagedInstance{}
	for pages := 0; pages < 20; pages++ {
		page, e := c.Client.DescribeInstances(ctx, &ec2.DescribeInstancesInput{Filters: []types.Filter{{Name: aws.String("tag:" + tagBackend), Values: []string{c.BackendID}}}, NextToken: token, MaxResults: aws.Int32(100)})
		if e != nil {
			return nil, errors.New("Spot inventory could not be authenticated")
		}
		for _, r := range page.Reservations {
			for _, v := range r.Instances {
				owned, tok := false, ""
				for _, tag := range v.Tags {
					switch aws.ToString(tag.Key) {
					case tagBackend:
						owned = aws.ToString(tag.Value) == c.BackendID
					case tagToken:
						tok = aws.ToString(tag.Value)
					}
				}
				if !owned {
					continue
				}
				state, reason := "unknown", ""
				if v.State != nil {
					state = string(v.State.Name)
				}
				if v.StateReason != nil {
					reason = aws.ToString(v.StateReason.Code)
				}
				out = append(out, spot.ManagedInstance{Instance: spot.Instance{ID: aws.ToString(v.InstanceId), State: state, Reason: reason, Owned: true}, Token: tok})
			}
		}
		if page.NextToken == nil || aws.ToString(page.NextToken) == "" {
			return out, nil
		}
		token = page.NextToken
	}
	return nil, errors.New("Spot inventory exceeds its bounded scan")
}
