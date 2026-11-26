# Spot runner validation

Validate the optional fleet in your own dedicated test account before enabling
automatic admissions. Repository examples and IDs in this source tree are
synthetic; this document does not establish that a particular deployment is ready.

## Local checks

Run the complete race-enabled Go suite, `go vet ./...`, the Node integration
tests, installer Python tests and the console build described in the README.
Validate CloudFormation with `cfn-lint` and workflow syntax with `actionlint`.
The lifecycle suite covers the global three-allocation cap, uncertain dispatches,
interruption, immutable retries, expiry and cleanup. Admin tests cover stale
heartbeats, repository scope, audited changes and disabled admissions.

On a disposable Linux ARM64 Docker host, exercise bootstrap privilege and
metadata isolation without an AWS instance or real runner registration:

```sh
SLUICEWORK_TEST_SPOT_BOOTSTRAP=1 go test ./internal/awsspot \
  -run TestUbuntuBootstrapPrivilegesAndMetadataFirewall -count=1 -v
```

The test uses a fake one-use runner archive and isolated networking. It verifies
that root can reach fake IMDS listeners while the runner cannot, private grants
are unreadable, inherited sudo cannot elevate the runner, and firewall errors
fail closed. Package installation, shutdown and service registration are stubbed.
This does not substitute for AWS IAM, routing, capacity or systemd validation.

## Bounded live canary

1. Review [fleet provisioning](priority-routing.md#provision-once). Use an explicit
   account, region, backend and restricted private runner group. Preserve
   `NetworkMode=existing` during an existing fleet's first template upgrade;
   changing networks requires disabling admissions and draining reservations.
2. Keep other admissions disabled, cap the test at one VM, and select a short
   runtime deadline and explicit bid ceiling. Include disk/IP/transfer charges.
3. Dispatch one trusted build. Verify the exact runner, source-bound artifacts,
   non-root execution, metadata isolation and independent shutdown deadline.
4. Verify the VM is terminated and its root disk, JIT registration and temporary
   resources are removed before reusing capacity or enabling more admissions.
   Preserve uncertain reservations until their original allocation is reconciled.

Record the reviewed configuration, test result and cleanup evidence privately.
Never publish App keys, JIT grants, EC2 user data, cloud-init logs or queue bodies.
Benchmark peak memory: a 4 GiB Standard runner may be too small for some builds.
