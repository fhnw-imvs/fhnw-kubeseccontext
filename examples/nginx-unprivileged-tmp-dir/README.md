# Example hardening of an unprivileged nginx container

This example is based on the unprivileged [nginx image](https://hub.docker.com/r/nginxinc/nginx-unprivileged)

It is deployed without any restrictions present, but as it binds to port 8080 it can run as non-root user, it also binds an emptyDir to `/tmp/` where all the nginx temp
files are created in

This setup shouldn't fail any tests, and get a fully hardened recommendation.