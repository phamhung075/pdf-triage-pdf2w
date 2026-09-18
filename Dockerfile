FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod ./
COPY canonicalpath/ ./canonicalpath/
COPY cleantext/ ./cleantext/
COPY cmd/ ./cmd/
RUN go build -o /pdf-triage-pdf2w ./cmd/server

FROM alpine:3.19
COPY --from=build /pdf-triage-pdf2w /pdf-triage-pdf2w
EXPOSE 3985
ENTRYPOINT ["/pdf-triage-pdf2w"]
