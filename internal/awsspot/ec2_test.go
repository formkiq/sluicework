package awsspot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"os"
	"os/exec"
	"path/filepath"
	"sluicework/internal/spot"
	"strings"
	"testing"
	"time"
)

type fakeEC2 struct {
	launch      *ec2.RunInstancesInput
	instances   []types.Instance
	terminated  []string
	err         error
	group       *types.SecurityGroup
	subnet      *types.Subnet
	groupError  error
	subnetError error
}

func outboundOnly() types.SecurityGroup {
	rule := func(port int32) types.IpPermission {
		return types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(port), ToPort: aws.Int32(port), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}
	}
	return types.SecurityGroup{GroupId: aws.String("sg-00000000000000001"), VpcId: aws.String("vpc-00000000000000001"), IpPermissionsEgress: []types.IpPermission{rule(80), rule(443)}}
}
func (f *fakeEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if f.groupError != nil {
		return nil, f.groupError
	}
	g := outboundOnly()
	if f.group != nil {
		g = *f.group
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: []types.SecurityGroup{g}}, nil
}
func (f *fakeEC2) DescribeSubnets(context.Context, *ec2.DescribeSubnetsInput, ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	if f.subnetError != nil {
		return nil, f.subnetError
	}
	n := types.Subnet{SubnetId: aws.String("subnet-00000000000000001"), VpcId: aws.String("vpc-00000000000000001")}
	if f.subnet != nil {
		n = *f.subnet
	}
	return &ec2.DescribeSubnetsOutput{Subnets: []types.Subnet{n}}, nil
}

func (f *fakeEC2) RunInstances(_ context.Context, in *ec2.RunInstancesInput, _ ...func(*ec2.Options)) (*ec2.RunInstancesOutput, error) {
	f.launch = in
	return &ec2.RunInstancesOutput{Instances: []types.Instance{{InstanceId: aws.String("i-00000000000000001")}}}, f.err
}
func (f *fakeEC2) DescribeImages(context.Context, *ec2.DescribeImagesInput, ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	return &ec2.DescribeImagesOutput{Images: []types.Image{{Architecture: types.ArchitectureValuesArm64, State: types.ImageStateAvailable, RootDeviceName: aws.String("/dev/sda1"), RootDeviceType: types.DeviceTypeEbs, OwnerId: aws.String("099720109477")}}}, nil
}
func (f *fakeEC2) DescribeInstances(context.Context, *ec2.DescribeInstancesInput, ...func(*ec2.Options)) (*ec2.DescribeInstancesOutput, error) {
	return &ec2.DescribeInstancesOutput{Reservations: []types.Reservation{{Instances: f.instances}}}, f.err
}
func (f *fakeEC2) TerminateInstances(_ context.Context, in *ec2.TerminateInstancesInput, _ ...func(*ec2.Options)) (*ec2.TerminateInstancesOutput, error) {
	f.terminated = append(f.terminated, in.InstanceIds...)
	return &ec2.TerminateInstancesOutput{}, nil
}
func spec() spot.LaunchSpec {
	p, _ := spot.LookupProfile("standard")
	return spot.LaunchSpec{ClientToken: strings.Repeat("a", 64), Name: "sluicework-spot-one", Label: "sluicework-spot-one", BackendID: "test", Job: spot.Job{ID: "one", Kind: "site", Repository: "team/site", WorkflowRef: "team/site/.github/workflows/sluicework-build.yml@refs/heads/main", WorkflowSHA: strings.Repeat("b", 40), SourceSHA: strings.Repeat("c", 40), RequestID: "123-1-one-local", Policy: spot.Policy{Profile: "standard", QueueMinutes: 5, StartupMinutes: 10, RuntimeMinutes: 90}}, Profile: p, ImageID: "ami-00000000000000001", RootDeviceName: "/dev/sda1", SubnetID: "subnet-00000000000000001", SecurityGroupID: "sg-00000000000000001", VolumeGiB: 32, Expires: time.Now().Add(90 * time.Minute), Package: spot.Package{URL: "https://github.com/actions/runner/releases/download/v2.338.2/actions-runner-linux-arm64-2.338.2.tar.gz", SHA256: strings.Repeat("a", 64)}, JIT: "T05FX0pPQl9TRUNSRVQ="}
}
func TestLaunchIsSingleJobSpotWithNoAWSIdentityAndEncryptedDisposableDisk(t *testing.T) {
	f := &fakeEC2{}
	c := &EC2Cloud{Client: f, BackendID: "test", AccountID: "123456789012"}
	s := spec()
	if _, e := c.Launch(context.Background(), s); e != nil {
		t.Fatal(e)
	}
	v := f.launch
	if aws.ToInt32(v.MinCount) != 1 || aws.ToInt32(v.MaxCount) != 1 || v.InstanceMarketOptions.MarketType != types.MarketTypeSpot || v.InstanceMarketOptions.SpotOptions.SpotInstanceType != types.SpotInstanceTypeOneTime || v.IamInstanceProfile != nil || v.KeyName != nil || v.LaunchTemplate != nil {
		t.Fatal("unexpected machine identity or capacity", v)
	}
	if v.MetadataOptions.HttpTokens != types.HttpTokensStateRequired || aws.ToInt32(v.MetadataOptions.HttpPutResponseHopLimit) != 1 || v.InstanceInitiatedShutdownBehavior != types.ShutdownBehaviorTerminate {
		t.Fatal("missing independent shutdown/metadata policy")
	}
	disk := v.BlockDeviceMappings[0].Ebs
	if !aws.ToBool(disk.Encrypted) || !aws.ToBool(disk.DeleteOnTermination) || disk.VolumeType != types.VolumeTypeGp3 {
		t.Fatal("disk retained or unencrypted")
	}
	script, e := base64.StdEncoding.DecodeString(aws.ToString(v.UserData))
	if e != nil {
		t.Fatal(e)
	}
	tmp := filepath.Join(t.TempDir(), "bootstrap.sh")
	if e = os.WriteFile(tmp, script, 0600); e != nil {
		t.Fatal(e)
	}
	if out, e := exec.Command("bash", "-n", tmp).CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	for _, unsafe := range []string{"set -x", "AWS_ACCESS_KEY_ID", "privateKey", "github-app.pem", "NOPASSWD"} {
		if strings.Contains(string(script), unsafe) {
			t.Fatal("bootstrap exposed reusable credentials", unsafe)
		}
	}
	if !strings.Contains(string(script), "ACTIONS_RUNNER_HOOK_JOB_STARTED") || !strings.Contains(string(script), "shutdown -h") || !strings.Contains(string(script), "hashlib.sha256") {
		t.Fatal("bootstrap missing gate, checksum or expiry")
	}
	for _, guard := range []string{"--uid-owner runner -d 169.254.169.254", "--uid-owner runner -d fd00:ec2::254"} {
		if !strings.Contains(string(script), guard) {
			t.Fatal("bootstrap lets workflow steps read instance metadata", guard)
		}
	}
	for _, guard := range []string{"'setpriv','--no-new-privs'", "NoNewPrivileges=yes", "usermod --gid runner --groups '' runner", "$(id -u runner) -ne 0"} {
		if !strings.Contains(string(script), guard) {
			t.Fatal("bootstrap does not constrain custom-image runner privileges", guard)
		}
	}
	if len(script) > 16384 {
		t.Fatal("bootstrap exceeds its size limit")
	}
}
func TestLaunchRefusesExposedOrMisplacedNetwork(t *testing.T) {
	inbound := outboundOnly()
	inbound.IpPermissions = []types.IpPermission{{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(22), ToPort: aws.Int32(22), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}}
	allTraffic := outboundOnly()
	allTraffic.IpPermissionsEgress = []types.IpPermission{{IpProtocol: aws.String("-1"), IpRanges: []types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}}
	portRange := outboundOnly()
	portRange.IpPermissionsEgress[1].ToPort = aws.Int32(8443)
	groupPeer := outboundOnly()
	groupPeer.IpPermissionsEgress[1].UserIdGroupPairs = []types.UserIdGroupPair{{GroupId: aws.String("sg-00000000000000002")}}
	noEgress := outboundOnly()
	noEgress.IpPermissionsEgress = nil
	noHTTPS := outboundOnly()
	noHTTPS.IpPermissionsEgress = noHTTPS.IpPermissionsEgress[:1]
	noRange := outboundOnly()
	noRange.IpPermissionsEgress[1].IpRanges = nil
	badRange := outboundOnly()
	badRange.IpPermissionsEgress[1].IpRanges[0].CidrIp = aws.String("not-a-cidr")
	prefixList := outboundOnly()
	prefixList.IpPermissionsEgress[1].PrefixListIds = []types.PrefixListId{{PrefixListId: aws.String("pl-example")}}
	otherVPC := types.Subnet{SubnetId: aws.String("subnet-00000000000000001"), VpcId: aws.String("vpc-00000000000000002")}
	for name, f := range map[string]*fakeEC2{"inbound": {group: &inbound}, "all traffic": {group: &allTraffic}, "port range": {group: &portRange}, "group peer": {group: &groupPeer}, "no egress": {group: &noEgress}, "no https": {group: &noHTTPS}, "no address range": {group: &noRange}, "bad address range": {group: &badRange}, "prefix list": {group: &prefixList}, "other vpc": {subnet: &otherVPC}, "group lookup failed": {groupError: errors.New("fixture lookup unavailable")}, "subnet lookup failed": {subnetError: errors.New("fixture lookup unavailable")}} {
		_, e := (&EC2Cloud{Client: f, BackendID: "test"}).Launch(context.Background(), spec())
		if !errors.Is(e, spot.ErrLaunchRejected) || f.launch != nil {
			t.Fatal("exposed network reached EC2:", name, e)
		}
	}
}

func TestSpotNetworkTemplateRequiresExplicitMigrationAndMetadataLimit(t *testing.T) {
	raw, e := os.ReadFile("../../infrastructure/priority-queue/spot.json")
	if e != nil {
		t.Fatal(e)
	}
	var template struct {
		Parameters map[string]map[string]any `json:"Parameters"`
		Resources  map[string]struct {
			Properties struct {
				PolicyDocument struct {
					Statement []struct {
						Sid       string
						Condition map[string]map[string]any
					}
				}
			}
		}
	}
	if e = json.Unmarshal(raw, &template); e != nil {
		t.Fatal(e)
	}
	mode, ok := template.Parameters["NetworkMode"]
	if !ok {
		t.Fatal("network mode must be an explicit deployment choice")
	}
	if _, ok := mode["Default"]; ok {
		t.Fatal("first stack update could silently migrate the active network")
	}
	found := false
	for _, statement := range template.Resources["RunnerPermissions"].Properties.PolicyDocument.Statement {
		if statement.Sid == "BoundedSpotInstanceTypes" {
			found = statement.Condition["NumericEquals"]["ec2:MetadataHttpPutResponseHopLimit"] == "1"
		}
	}
	if !found {
		t.Fatal("IAM must require IMDS hop limit 1 independently of application code")
	}
}
func TestInvalidBootstrapCannotReachEC2(t *testing.T) {
	for _, mutate := range []func(*spot.LaunchSpec){func(s *spot.LaunchSpec) { s.Package.URL = "https://evil.example/runner" }, func(s *spot.LaunchSpec) { s.JIT = "raw\nheredoc" }, func(s *spot.LaunchSpec) { s.Profile.CPUs = 8 }, func(s *spot.LaunchSpec) { s.Job.WorkflowRef = "other/repo/whatever" }} {
		f := &fakeEC2{}
		s := spec()
		mutate(&s)
		_, e := (&EC2Cloud{Client: f, BackendID: "test"}).Launch(context.Background(), s)
		if !errors.Is(e, spot.ErrLaunchRejected) || f.launch != nil {
			t.Fatal("unsafe launch reached AWS", e)
		}
	}
}
func TestTerminationRefusesUnownedMachinesAndRecognizesOnlySpotReasons(t *testing.T) {
	f := &fakeEC2{instances: []types.Instance{{InstanceId: aws.String("i-00000000000000001"), State: &types.InstanceState{Name: types.InstanceStateNameRunning}}}}
	c := &EC2Cloud{Client: f, BackendID: "test"}
	if e := c.Terminate(context.Background(), "i-00000000000000001"); e == nil || len(f.terminated) > 0 {
		t.Fatal("unowned machine terminated")
	}
	if Interrupted("Client.InstanceInitiatedShutdown") || !Interrupted("Server.SpotInstanceTermination") {
		t.Fatal("normal shutdown misclassified")
	}
}
func TestJobGateBindsEveryImmutableIdentityBeforeWorkflowSteps(t *testing.T) {
	python, e := exec.LookPath("python3")
	if e != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "gate.py")
	if e = os.WriteFile(file, []byte(jobStartHook), 0600); e != nil {
		t.Fatal(e)
	}
	test := `import runpy,copy
f=runpy.run_path(__import__('sys').argv[1])['authorized']
e={'job':{'repository':'team/site','workflowRef':'team/site/.github/workflows/sluicework-build.yml@refs/heads/main','workflowSha':'abc','sourceSha':'def','requestId':'123-local','kind':'site'},'label':'spot-one'}
env={'GITHUB_REPOSITORY':'team/site','GITHUB_WORKFLOW_REF':e['job']['workflowRef'],'GITHUB_SHA':'abc','GITHUB_EVENT_NAME':'workflow_dispatch'}
event={'inputs':{'request_id':'123-local','source_sha':'def','build_kind':'site','host':'local','local_platform':'linux-arm64','machine_label':'spot-one'}}
assert f(env,event,e)
for key in env:
 bad=dict(env);bad[key]='foreign';assert not f(bad,event,e),key
for key in event['inputs']:
 bad=copy.deepcopy(event);bad['inputs'][key]='foreign';assert not f(env,bad,e),key
assert not f(env,{},e)
`
	if out, e := exec.Command(python, "-c", test, file).CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
}
