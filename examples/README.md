# Example workloads

This directory contains a few different worklodas used to test the Orakel of Funk.

They consist of a mix of check specific workloads, as well as regular applications that can be used on a Kubernetes cluster.

## Regular workloads

The following regular workloads are deployed for verifying the functionality of the Orakel.

### Mariadb

This is a Mariadb deployment based on the official Helm chart.

The rendered manifest was created using the default `values.yaml` and just setting the namespace flag for Helm.

The resulting StatefulSet already contains a restricted securityContext and accordingly the Orakel won't find any improvements to be done.

### nging-(privileged/unprivileged/unprivileged-tmp-dir)

These are three example workloads based on nginx, as this is a commonly misconfigured pod, which is be easy to configure properly, if you know what to do.

The `privileged` version is running the regular nginx image without any modifications, this results in nginx beeing started on port 80, and the 
main process running as root, but delegating to the nginx/www-data user. This check will fail the `RunAsNonRoot`, `ReadOnlyRootFilesystems` and 
`DropCapabilitiesAll` checks.

The `unprivileged` version contains a ConfigMap reconfiguring nginx to write its pid file, the error log, and all its temporary files, to `/tmp` 
(instead of `/etc/nginx`) as well as binding to port 8080. This check will pass the `RunAsNonRoot` check, but still fail `ReadOnlyRootFilesystems` and 
`DropCapabilitiesAll` checks as it performs a `chown` in one of the startup scripts.

The `unprivileged-tmp-dir` variant, also mounts an `emptyDir` into `/tmp` which solves the issues in the `ReadOnlyRootFilesystem` check.


## Check specific workloads

These workloads consist of a small script which is run in a pod, and either fails or is successfull.

If the script is successfull, it will sleep for 10 minutes, before exiting and triggering a restart. 

The goal of these check specific workloads is to verify if a single check will trigger, but they could also be combined to build more complex workloads,
which require very specific configurations.

Since the Orakel currently only performs a "drop all capabilities" check, not every capability has it's own check deployment, but when extending the functionality 
to check each capability specifically a test deployment for each capability must be added as well.

### chown / CAP_CHOWN

This check creates a file in `/tmp/` (which is a mounted `emptyDir`), and then tries to chown the file to a different user.

The target user is www-data(33) which exsists in the default debian:bookworm image, but changing a file to arbitrary user requires the `CAP_CHOWN` capability.

We expect this test to fail for the `DropCapabilitiesAll` check as well as the `RunAsNonRoot` check. 

### port-check-(un)privileged

These checks use `ncat` to listen/bind on a specific port. Usually all ports below 1000 are considered privileged ports, and the assumption was that the `privileged` variant
will fail the `DropCapabilitiesAll` check as well as `RunAsNonRoot`, but depending on the container runtime used, `ip_unprivileged_port_start` is set to `0` which allows binding 
to all ports for non-privileged users.

This was enabled in containerd 2.0 as default (https://github.com/containerd/containerd/pull/9348) but depends on the Kernel version, after docker/moby already enabled this in 2020 
(https://github.com/moby/moby/pull/41030). Kubernetes itself doesn't currently enforce a consistent state between container runtimes, which makes this more difficult to test than expected.

As of right now, using Containerd 2.0 with Kernel 4.11+, or docker as teh container runtime, will make both tests pass all checks, but using cri-o or older containerd version will make it fail.

### privilege-escalation

This deployment checks if it can escalate its privleges, using the `setpriv -d` command, which returns the privileges and capabilties for the current user, one of which is a boolean flag indication
the privilege escalation.

This deployment fails the `PrivilegeEscalation` check, and requires the `allowPrivilegeEscalation` flag to false.

### readonlyrootfs-(failing|success)

These deployments either pass or fail the `readOnlyRootFilesystem` check. Both try to create a file in `/tmp/`, but the `success` variante has an `emptyDir` mounted, while the `failing` one doesn't.

Using an `emptyDir` to allow writting to specific directories is a common pattern in Kubernetes, so it's important that we have proper checks for this behaviour.
