# Example hardening of an unprivileged nginx container

This example is based on the unprivileged [nginx image](https://hub.docker.com/r/nginxinc/nginx-unprivileged)

It is deployed without any restrictions present, but as it binds to port 8080 it can run as non-root user

We expect the following tests to fail
- readOnlyRootFileSystem
- dropCapabilitiesAll