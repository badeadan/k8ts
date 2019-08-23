# k8ts

Kubernetes Tombstone (k8ts) is a tool that can be used to save all
logs that would otherwise be deleted by Kubernetes after their
corresponding containers go away.

k8ts saves log files from /var/log/containers before they are deleted
to /var/log/tombstone. If you want to see what happened with a container
that is no longer running you can search for its logs in /var/log/containers
or /var/log/tombstone.

Note that the storage space in /var/log/tombstone is not bounded and
will grow over time as it contains logs for all containers that were
executed on the current machine.

## Description

This tool was developed in the context of StarlingX so it's tailored
for that environment: remote deployment over ssh, systemd service
install, monitor logs in `/var/log/containers`.

In a nutshell it:
* copies itself to the remote host
* configures a systemd service
* uses inotify to listen for open's in `/var/log/containers`
* keeps the file descriptor open
* when file is removed from `/var/log/containers` it rewinds the file
  and writes a copy of it to `/var/log/tombstone`
* closes the file descriptor

## Usage

k8ts contains 3 utilities that can be used deploy, install/uninstall
a corresponding systemd service and monitor logs in `/var/log/containers`.

### Deploy

k8ts can deploy itself as a service to a remote machine using ssh.
This ensures that k8ts keeps running after target host reboots until
the service is uninstalled.

The target is `user:password@host#port` or a simple host if the key is
also provided. An optional ssh proxy (next hop) is also supported.

The rest of the command line options are forwarded to `k8ts monitor` via
install. Read log monitoring section for more details. 

```
usage: k8ts deploy -t|--target "<value>" [-k|--target-key "<value>"]
            [-p|--proxy "<value>"] [-q|--proxy-key "<value>"] [-i|--include-log
            "<value>"] [-e|--exclude-log "<value>"] [-s|--skip-conversion]
            [-h|--help]

            Deploy k8ts on a remote host via SSH

Arguments:

  -t  --target           Where to deploy k8ts
  -k  --target-key       SSH key to use when connecting to taget
  -p  --proxy            Next hop (proxy) used to reach target host
  -q  --proxy-key        SSH key to use when connecting to proxy
  -i  --include-log      Preserve logs of pods matching this pattern.
  -e  --exclude-log      Ignore logs of pods matching this pattern.
  -s  --skip-conversion  Do not convert logs from JSON to text.
  -h  --help             Print help information
```

Example:
```
k8ts deploy -t user:password@target-ip#port
```

### Service management

k8ts integrates with systemd and it can install/uninstall itself as a
service. Command line options are forwarded to `k8ts monitor`. Read
log monitoring section for more details.

```
usage: k8ts service <Command> [-i|--include-log "<value>"] [-e|--exclude-log
            "<value>"] [-k|--keep-if "<value>"] [-s|--skip-conversion]
            [-h|--help]

            Control k8ts service running on this host

Commands:

  install    Install service
  uninstall  Uninstall service

Arguments:

  -i  --include-log      Preserve logs of pods matching this pattern.
  -e  --exclude-log      Ignore logs of pods matching this pattern.
  -k  --keep-if          Keep logs only if content matches this pattern.
  -s  --skip-conversion  Do not convert logs from JSON to text.
  -h  --help             Print help information
```

This command is usually invoked by `k8ts deploy` so there is no need
to run it manually.

Example:
```
k8ts install
```

### Log monitoring

Log monitoring supports filters to:
* keep only files whose name match a specific pattern (using
  `--include-log`)
* ignore files whose name match a specific pattern (using
  `--exclude-log`). Content of this files will be lost
* keep log files if they contain a specific pattern (using
  `--keep-if`)
  
By default logs are converted from JSON to plain text but this
can be disabled using `--skip-conversion` option.

```
usage: k8ts monitor [-i|--include-log "<value>"] [-e|--exclude-log "<value>"]
            [-k|--keep-if "<value>"] [-s|--skip-conversion] [-h|--help]

            Monitor kubernetes pod logs

Arguments:

  -i  --include-log      Preserve logs of pods matching this pattern.
  -e  --exclude-log      Ignore logs of pods matching this pattern.
  -k  --keep-if          Keep logs only if content matches this pattern.
  -s  --skip-conversion  Do not convert logs from JSON to text.
  -h  --help             Print help information
```

Example:
```
k8ts monitor
```

## Build

To build k8ts you need GNU Make and optionally `upx` to shrink the
resulting binary size:
```
make
```
