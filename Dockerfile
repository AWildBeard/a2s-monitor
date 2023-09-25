FROM golang:latest as builder
WORKDIR /app
COPY ["Makefile", "go.*", "*.go", "./"]
RUN make release build
WORKDIR /dist
RUN cp /app/app ./app
RUN ldd app | tr -s '[:blank:]' '\n' | grep '^/' | \
    xargs -I % sh -c 'mkdir -p $(dirname ./%); cp % ./%;' \
RUN mkdir -p lib64 && cp /lib64/ld-linux-x86-64.so.2 lib64/

FROM scratch
COPY --chown=0:0 --from=builder /dist /
USER 65534
ENTRYPOINT ["/app"]