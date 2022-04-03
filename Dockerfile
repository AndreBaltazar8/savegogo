FROM golang:1.18 AS builder
WORKDIR /
COPY go.mod ./
COPY go.sum ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o app .

FROM alpine:latest
RUN apk --no-cache add ca-certificates
WORKDIR /
COPY --from=builder /app ./
COPY /index.html ./index.html
COPY /static ./static
RUN mkdir /data
RUN touch /data/state.json
RUN chmod 0777 /data/state.json
VOLUME /data
EXPOSE 4446
CMD ["./app"]
USER nobody:nobody
