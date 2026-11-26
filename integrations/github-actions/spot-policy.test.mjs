import { readFileSync } from 'node:fs'
import assert from 'node:assert/strict'
import test from 'node:test'
const template = JSON.parse(
  readFileSync(new URL('../../infrastructure/priority-queue/spot.json', import.meta.url)),
)
test('Spot infrastructure is optional, outbound-only and never provisions a standing runner', () => {
  const resources = Object.values(template.Resources)
  assert.ok(
    !resources.some((r) =>
      [
        'AWS::EC2::Instance',
        'AWS::AutoScaling::AutoScalingGroup',
        'AWS::EC2::NatGateway',
        'AWS::IAM::InstanceProfile',
      ].includes(r.Type),
    ),
  )
  const sg = template.Resources.RunnerSecurityGroup.Properties
  assert.deepEqual(sg.SecurityGroupIngress, [])
  assert.deepEqual(
    sg.SecurityGroupEgress.map((r) => [r.FromPort, r.ToPort]),
    [
      [80, 80],
      [443, 443],
    ],
  )
  const statements = template.Resources.RunnerPermissions.Properties.PolicyDocument.Statement
  assert.ok(
    !statements.some((s) => s.Action.includes('iam:PassRole') || s.Action.includes('ec2:*')),
  )
  const instance = statements.find((s) => s.Sid === 'BoundedSpotInstanceTypes')
  assert.deepEqual(instance.Condition.StringEquals['ec2:InstanceType'], [
    'c6gd.medium',
    'c8gn.large',
  ])
  assert.equal(instance.Condition.StringEquals['ec2:InstanceMarketType'], 'spot')
  assert.equal(instance.Condition.StringEquals['ec2:MetadataHttpTokens'], 'required')
  assert.equal(instance.Condition.Null['ec2:InstanceProfile'], 'true')
  const terminate = statements.find((s) => s.Sid === 'TerminateThisFleetOnly')
  assert.ok(terminate.Condition.StringEquals['ec2:ResourceTag/sluicework:backend'])
})
