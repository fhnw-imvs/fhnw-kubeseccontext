# Example hardening of an unrestricted nginx container

This example is based on the official [docker nginx image](https://hub.docker.com/_/nginx)

It must run as root since it binds to port 80, and it requires write access to the container filesystem as it creates a PID file.

We expect the following tests to fail
- runAsNonRoot
- readOnlyRootFileSystem