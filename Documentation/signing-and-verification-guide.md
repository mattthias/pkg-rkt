# Signing and Verification Guide

This guide will walk you through signing, distributing, and verifying the hello ACI created in the [getting started guide](getting-started-guide.md).

```
hello-0.0.1-linux-amd64.aci
```

* [Signing ACIs](#signing-acis)
* [Distributing Images via Meta Discovery](#distributing-images-via-meta-discovery)
* [Verifying Images with rkt](#verifying-images-with-rkt)
* [Establishing Trust](#establishing-trust)
* [Example Usage](#example-usage)

## Signing ACIs

By default rkt requires ACIs to be signed using a gpg detached signature. The following steps will walk you through the creation of a gpg keypair suitable for signing an ACI. If you have an existing gpg signing key skip to the [Signing the ACI](signing-the-aci) step.  

### Generate a gpg signing key

Create a file named `gpg-batch`

```
%echo Generating a default key
Key-Type: RSA 
Key-Length: 2048
Subkey-Type: RSA 
Subkey-Length: 2048
Name-Real: Kelsey Hightower
Name-Comment: ACI signing key
Name-Email: kelsey.hightower@coreos.com
Expire-Date: 0
Passphrase: rkt
%pubring rkt.pub
%secring rkt.sec
%commit
%echo done
```

#### Generate the key using batch mode

```
$ gpg --batch --gen-key gpg-batch
```

#### List the keys

```
$ gpg --no-default-keyring --secret-keyring ./rkt.sec --keyring ./rkt.pub --list-keys
```
```
./rkt.pub
------------
pub   2048R/26EF7A14 2015-01-09
uid       [ unknown] Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>
sub   2048R/B9C074CD 2015-01-09
```

From the output above the level of trust for the signing key is unknown. This will cause the following warning if we attempt to validate an ACI signed with this key using the gpg cli:

```
gpg: WARNING: This key is not certified with a trusted signature!
```

Since we know exactly where this key came from let's trust it:

```
$ gpg --no-default-keyring --secret-keyring ./rkt.sec --keyring ./rkt.pub --edit-key 26EF7A14 trust
gpg (GnuPG/MacGPG2) 2.0.22; Copyright (C) 2013 Free Software Foundation, Inc.
This is free software: you are free to change and redistribute it.
There is NO WARRANTY, to the extent permitted by law.

Secret key is available.

pub  2048R/26EF7A14  created: 2015-01-09  expires: never       usage: SC  
                     trust: unknown       validity: unknown
sub  2048R/B9C074CD  created: 2015-01-09  expires: never       usage: E   
[ unknown] (1). Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>

Please decide how far you trust this user to correctly verify other users' keys
(by looking at passports, checking fingerprints from different sources, etc.)

  1 = I don't know or won't say
  2 = I do NOT trust
  3 = I trust marginally
  4 = I trust fully
  5 = I trust ultimately
  m = back to the main menu

Your decision? 5
Do you really want to set this key to ultimate trust? (y/N) y
                                                             
pub  2048R/26EF7A14  created: 2015-01-09  expires: never       usage: SC  
                     trust: ultimate      validity: unknown
sub  2048R/B9C074CD  created: 2015-01-09  expires: never       usage: E   
[ unknown] (1). Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>
Please note that the shown key validity is not necessarily correct
unless you restart the program.

gpg> quit
```

#### Export the public key

```
$ gpg --no-default-keyring --armor \
--secret-keyring ./rkt.sec --keyring ./rkt.pub \
--export kelsey.hightower@coreos.com > pubkeys.gpg
```

### Signing the ACI

```
$ gpg --no-default-keyring --armor \
--secret-keyring ./rkt.sec --keyring ./rkt.pub \
--output hello-0.0.1-linux-amd64.aci.asc \
--detach-sig hello-0.0.1-linux-amd64.aci
```

#### Verify the image using gpg

```
$ gpg --no-default-keyring \
--secret-keyring ./rkt.sec --keyring ./rkt.pub \
--verify hello-0.0.1-linux-amd64.aci.asc hello-0.0.1-linux-amd64.aci
```
```
gpg: Signature made Fri Jan  9 05:01:49 2015 PST using RSA key ID 26EF7A14
gpg: Good signature from "Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>" [ultimate]
```

At this point you should have the following three files:

```
hello-0.0.1-linux-amd64.aci.asc
hello-0.0.1-linux-amd64.aci
pubkeys.gpg
```

## Distributing Images via Meta Discovery

Host an HTML page with the following meta tags:

```
<!DOCTYPE html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="ac-discovery" content="example.com/hello https://example.com/images/{name}-{version}-{os}-{arch}.{ext}">
    <meta name="ac-discovery-pubkeys" content="example.com/hello https://example.com/pubkeys.gpg">
  <head>
<html>
```

Serve the following files at the locations described in the meta tags:

```
https://example.com/images/hello-0.0.1-linux-amd64.aci.asc
https://example.com/images/hello-0.0.1-linux-amd64.aci
https://example.com/pubkeys.gpg
```

### rkt Integration

Lets walk through the steps rkt takes when fetching images using Meta Discovery.
The following rkt command:

```
$ rkt run example.com/hello:0.0.1
```

results in rkt retrieving the following URIs:

```
https://example.com/hello?ac-discovery=1
https://example.com/images/example.com/hello-0.0.1-linux-amd64.aci
https://example.com/images/example.com/hello-0.0.1-linux-amd64.aci.asc
```

The first response contains the template URL used to download the ACI image and detached signature file.

```
<meta name="ac-discovery" content="example.com/hello https://example.com/images/{name}-{version}-{os}-{arch}.{ext}">
```

rkt populates the `{os}` and `{arch}` based on the current running system.
The `{version}` will be taken from the tag given on the command line or "latest" if not supplied.
The `{ext}` will be substituted appropriately depending on artifact being retrieved: .aci will be used for ACI images and .aci.asc will be used for detached signatures.

Once the ACI image has been downloaded rkt will extract the image's name from the image metadata. The image's name will be used to locate trusted public keys in the rkt keystore and perform signature validation.

## Verifying Images with rkt

### Establishing Trust

By default rkt does not trust any signing keys. Trust is established by storing public keys in the rkt keystore.
The following directories make up the default rkt keystore layout:

```
/etc/rkt/trustedkeys/root.d
/etc/rkt/trustedkeys/prefix.d
/usr/lib/rkt/trustedkeys/root.d
/usr/lib/rkt/trustedkeys/prefix.d
```

System administrators should store trusted keys under `/etc/rkt` as `/usr/lib/rkt` is designed to be used by the OS distribution.
Trusted keys are saved in the desired directory named after the fingerprint of the public key.
System administrators can "disable" a trusted key by writing an empty file under `/etc/rkt`.
For example, if your OS distribution shipped with the following trusted key:

```
/usr/lib/rkt/trustedkeys/prefix.d/coreos.com/a175e31de7e3c5b9d2c4603e4dfb22bf75ef7a23
```

you can disable it by writing the following empty file:

```
/etc/rkt/trustedkeys/prefix.d/coreos.com/a175e31de7e3c5b9d2c4603e4dfb22bf75ef7a23
```

### Trusting the example.com/hello key

#### Download the public key

```
$ curl -O https://example.com/pubkeys.gpg
```

#### Capture the public key fingerprint

```
$ gpg --no-default-keyring --fingerprint --keyring ./pubkeys.gpg kelsey.hightower@coreos.com
```

```
pub   2048R/26EF7A14 2015-01-09
      Key fingerprint = B346 E31D E7E3 C6F9 D1D4  603F 4DFB 61BF 26EF 7A14
uid       [ unknown] Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>
sub   2048R/B9C074CD 2015-01-09
```

Remove white spaces and convert to lowercase:

```
$ echo "B346 E31D E7E3 C6F9 D1D4  603F 4DFB 61BF 26EF 7A14" | \
  tr -d "[:space:]" | tr '[:upper:]' '[:lower:]'
```
```
b346e31de7e3c6f9d1d4603f4dfb61bf26ef7a14
```

#### Trust the key for the example.com/hello prefix

```
mkdir -p /etc/rkt/trustedkeys/prefix.d/example.com/hello
mv pubkeys.gpg  /etc/rkt/trustedkeys/prefix.d/example.com/hello/b346e31de7e3c6f9d1d4603f4dfb61bf26ef7a14
```

Now the public key with fingerprint `b346e31de7e3c6f9d1d4603f4dfb61bf26ef7a14` will be trusted for all images with a name prefix of `example.com/hello`.
If you would like to trust a public key for any image store the public key in one of the following directories:

```
/etc/rkt/trustedkeys/root.d
/usr/lib/rkt/trustedkeys/root.d
```

### Example Usage

```
$ sudo rkt -h
Usage of rkt:
  -debug=false: Print out more debug information to stderr
  -dir="/var/lib/rkt": rkt data directory
  -help=false: Print usage information and exit
  -insecure-skip-verify=false: skip image verification
```

#### Download, verify and run an ACI

By default rkt will attempt to download the ACI detached signature and verify the image:

```
$ sudo rkt run example.com/hello:0.0.1
rkt: starting to discover app img example.com/hello:0.0.1
rkt: starting to fetch img from http://example.com/images/example.com/hello-0.0.1-linux-amd64.aci
Downloading aci: [                                             ] 7.24 KB/1.26 MB
rkt: example.com/hello:0.0.1 verified signed by:
  Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>
/etc/localtime is not a symlink, not updating container timezone.
^]^]Container stage1 terminated by signal KILL.
```

Use the `-insecure-skip-verify` flag to disable image verification for a single run:

```
$ sudo rkt -insecure-skip-verify run example.com/hello:0.0.1
rkt: starting to discover app img example.com/hello:0.0.1
rkt: starting to fetch img from http://example.com/images/example.com/hello-0.0.1-linux-amd64.aci
rkt: warning: signature verification has been disabled
Downloading aci: [=                                            ] 32.8 KB/1.26 MB
/etc/localtime is not a symlink, not updating container timezone.
^]^]Container stage1 terminated by signal KILL.
```

Notice when the `-insecure-skip-verify` flag is used, rkt will print the following warning:

```
rkt: warning: signature verification has been disabled
```

#### Download and verify an ACI

Using the fetch subcommand you can download and verify an ACI without immediately running a pod.
This can be useful to precache ACIs on a large number of hosts:

```
$ sudo rkt fetch example.com/hello:0.0.1
rkt: starting to discover app img example.com/hello:0.0.1
rkt: starting to fetch img from http://example.com/images/example.com/hello-0.0.1-linux-amd64.aci
Downloading aci: [                                             ] 14.5 KB/1.26 MB
rkt: example.com/hello:0.0.1 verified signed by:
  Kelsey Hightower (ACI signing key) <kelsey.hightower@coreos.com>
sha512-b3f138e10482d4b5f334294d69ae5c40
```

As before, use the `-insecure-skip-verify` flag to disable image verification:

```
$ sudo rkt -insecure-skip-verify fetch example.com/hello:0.0.1
rkt: starting to discover app img example.com/hello:0.0.1
rkt: starting to fetch img from http://example.com/images/example.com/hello-0.0.1-linux-amd64.aci
rkt: warning: signature verification has been disabled
Downloading aci: [                                             ] 4.34 KB/1.26 MB
sha512-b3f138e10482d4b5f334294d69ae5c40
```
