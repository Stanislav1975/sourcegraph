# Adding SSL (HTTPS) to Sourcegraph with a self-signed certificate

This is for external Sourcegraph instances that need a self-signed certificate because they don't yet have a certificate from a [globally trusted Certificate Authority (CA)](https://en.wikipedia.org/wiki/Certificate_authority#Providers). It includes how to get the self-signed certificate trusted by your browser.

> NOTE: Using an IP address also works, including the browser being able to trust (validate) the SSL certificate.

Configuring NGINX with a self-signed certificate to support SSL requires:

1. [Installing mkcert](#1-installing-mkcert).
1. [Creating the self-signed certificate](#2-creating-the-self-signed-certificate)
1. [Configuring NGINX for SSL](#3-adding-ssl-support-to-nginx)
1. [Changing the Sourcegraph container to listen on port 443](#4-changing-the-quickstart-command-to-listen-on-port-for-ssl)
1. [Getting the self-signed certificate to be trusted (valid) on external instances](#5-getting-the-self-signed-certificate-to-be-trusted-valid-on-external-instances)

## 1. Installing mkcert

While the [OpenSSL](https://wiki.openssl.org/index.php/Command_Line_Utilities) CLI can can generate self-signed certificates, its API is challenging unless you're well versed in SSL.

A better alternative is [mkcert](https://github.com/FiloSottile/mkcert#mkcert), an abstraction over OpenSSL written by [Filippo Valsorda](https://github.com/FiloSottile), a cryptographer working at Google on the Go team.

> NOTE: The following commands are to be run on the Docker host, **not** inside the Sourcegraph container.

To set up mkcert on the Sourcegraph instance:

1. [Install mkcert](https://github.com/FiloSottile/mkcert#installation)
1. Create the root [CA](https://en.wikipedia.org/wiki/Certificate_authority) by running:

```shell
sudo CAROOT=~/.sourcegraph/config mkcert -install
```

## 2. Creating the self-signed certificate

Now that the root CA has been created, mkcert can issue a self-signed certificate (`sourcegraph.crt`) and key (`sourcegraph.key`).

```shell
sudo CAROOT=~/.sourcegraph/config mkcert \
  -cert-file ~/.sourcegraph/config/sourcegraph.crt \
  -key-file ~/.sourcegraph/config/sourcegraph.key \
  $HOSTNAME_OR_IP
```

Run `sudo ls -la ~/.sourcegraph/config` and you should see the CA and SSL certificates and keys.

## 3. Adding SSL support to NGINX

Change the [default `~/.sourcegraph/config/nginx.conf`](https://github.com/sourcegraph/sourcegraph/blob/master/cmd/server/shared/assets/nginx.conf) by:

**1.** Replacing `listen 7080;` with `listen 7080 ssl;`.

**2.** Adding the following two lines below the `listen 7080 ssl;` statement.

```nginx
ssl_certificate         sourcegraph.crt;
ssl_certificate_key     sourcegraph.key;
```

The `nginx.conf` should now look like:

```nginx
...
http {
    ...
    server {
       ...
        listen 7080 ssl;
        ssl_certificate         sourcegraph.crt;
        ssl_certificate_key     sourcegraph.key;
        ...
    }
}
```

## 4. Changing the Sourcegraph container to listen on port 443

> NOTE: If the Sourcegraph container is still running, stop it before reading on.

Now that NGINX is listening on port 443, we need the Sourcegraph container to listen on port 443 by adding `--publish 443:7080` to the `docker run` command:

```shell
docker container run \
  --rm  \
  --publish 7080:7080 \
  --publish 2633:2633 \
  --publish 443:7080 \
  \
  --volume ~/.sourcegraph/config:/etc/sourcegraph  \
  --volume ~/.sourcegraph/data:/var/opt/sourcegraph  \
  sourcegraph/server:3.9.1
```

> NOTE: We recommend removing `--publish 7080:7080` as it's not needed and traffic sent to that port is un-encrypted.

Run the new Docker command, then validate by opening your browser at `https://$HOSTNAME_OR_IP`.

If running Sourcegraph locally, the certificate will be valid because `mkcert` added the root CA to the list trusted by your OS.

## 5. Getting the self-signed certificate to be trusted (valid) on external instances

To have the browser trust the certificate, the root CA on the Sourcegraph instance must be installed locally by:

**1.** [Installing mkcert locally](https://github.com/FiloSottile/mkcert#installation)

**2.** Downloading `rootCA-key.pem` and `rootCA.pem` from `~/.sourcegraph/config/mkcert` on the Sourcegraph instance to the location of `mkcert -CAROOT` on your local machine:

```shell
# Run locally: Ensure directory the root CA files will be downloaded to exists
mkdir -p "$(mkcert -CAROOT)"
```

```shell
# Run on Sourcegraph host: Ensure `scp` user can read (and therefore download) the root CA files
sudo chown $USER ~/.sourcegraph/config/root*
```

```shell
# Run locally: Download the files (change username and hostname)
scp user@example.com:~/.sourcegraph/config/root* "$(mkcert -CAROOT)"
```

**3.** Install the root CA by running:

```shell
mkcert -install
```

Open your browser again at `https://$HOSTNAME_OR_IP` and this time, your certificate should be valid.

### Getting the self-signed cert trusted on other developer machines

This is largely the same as step 5, except easier. For other developer machines to trust the self-signed cert:

- [Install mkcert](https://github.com/FiloSottile/mkcert#installation).
- Download the `rootCA-key.pem` and `rootCA.pem` from Slack or other internal system.
- Move the `rootCA-key.pem` and `rootCA.pem` files into the `mkcert -CAROOT` directory on their machine.
- Run `mkcert -install` on their machine.

## Next steps

- [Configure Sourcegraph's `externalURL`](config/critical_config.md)
- [Redirect to external HTTPS URL](nginx.md#redirect-to-external-https-url)
- [NGINX HTTP Strict Transport Security](nginx.md#redirect-to-external-https-url)
- [NGINX SSL Termination guide](https://docs.nginx.com/nginx/admin-guide/security-controls/terminating-ssl-http/)
- [NGINX HTTPS Servers guide](https://nginx.org/en/docs/http/configuring_https_servers.html).
