FROM alpine:3.20
RUN apk add --no-cache netcat-openbsd
EXPOSE 8080
CMD while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 12\r\n\r\nhello piper\n' | nc -l -p 8080; done
