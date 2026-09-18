FROM golang:1.23-alpine AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /pdf-triage ./cmd/pdf-triage

FROM alpine:3.19
COPY --from=build /pdf-triage /pdf-triage
EXPOSE 3971
ENTRYPOINT ["/pdf-triage"]
CMD ["serve"]
