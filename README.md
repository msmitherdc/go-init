# go-init

**This is a hard fork of the original [go-init](https://github.com/pablo-ruth/go-init) intended to support future maintenance.**

**go-init** is a minimal init system with simple *lifecycle management* heavily inspired by [dumb-init](https://github.com/Yelp/dumb-init).

It is designed to run as the first process (PID 1) inside a container.

It is lightweight (around 2MB) and statically linked so you don't need to install any dependency.

## Download

You can download the latest version on the [releases page](https://github.com/msmitherdc/go-init/releases).

Binaries are published for Linux on `amd64`, `arm64`, `armv7`, `386`, `ppc64le`, `s390x` and `riscv64`. Each release also ships a `SHA256SUMS` file, so you can verify a download with:

```
sha256sum -c SHA256SUMS --ignore-missing
```

## Exit codes

**go-init** exits with the exit code of the main command, so a container's exit status reflects what actually happened inside it. A main command killed by a signal is reported as `128 + signal number`, the same convention POSIX shells use.

## Why you need an init system

I can't explain it better than Yelp in *dumb-init* repo, so please [read their explanation](https://github.com/Yelp/dumb-init/blob/v1.2.0/README.md#why-you-need-an-init-system)

Summary:
- Proper signal forwarding
- Orphaned zombies reaping

## Why another minimal init system

In addition to *init* problematic, **go-init** tries to solve another Docker flaw by adding *hooks* on start and stop of the main process.

If you want to launch a command before the main process of your container and another one after the main process exit, you can't with Docker, see [issue 6982](https://github.com/moby/moby/issues/6982)

With **go-init** you can do that with "pre" and "post" hooks.

## Usage

### one command

```
$ go-init -main "my_command param1 param2"
```

### pre-start and post-stop hooks

```
$ go-init -pre "my_pre_command param1" -main "my_command param1 param2" -post "my_post_command param1"
```

### Quoting

Commands are split on whitespace and executed directly, without a shell. Quotes and shell syntax are **not** interpreted, so `-main "echo 'hello world'"` passes two arguments, `'hello` and `world'`. If you need shell features such as quoting, pipes or variable expansion, invoke a shell explicitly and keep the script free of whitespace-separated quoting:

```
$ go-init -main "/bin/sh /path/to/script.sh"
```

## docker

Example of Dockerfile using *go-init*:
```
FROM alpine:latest

COPY go-init /bin/go-init

RUN chmod +x /bin/go-init

ENTRYPOINT ["go-init"]

CMD ["-pre", "echo hello world", "-main", "sleep 5", "-post", "echo I finished my sleep bye"]
```

Build it:
```
docker build -t go-init-example
```

Run it:
```
docker run go-init-example
```
