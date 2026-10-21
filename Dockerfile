FROM golang:1.27.3@sha256:f24164268fc1e741bbdc0066dbadcf1c8426c5a8489f0729c02c3905a53032fd AS builder

WORKDIR /magneto.internal

# Get the dependencies so it can be cached into a layer
COPY go.mod go.sum ./
RUN go mod download

# Now copy all the source...
COPY . .

# ...and build it.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ./bin/magneto \
    -ldflags="-s -w" \
    .

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

ARG UID=10001
ARG GID=10001
ARG USER_NAME=appuser
ARG GROUP_NAME=appgroup
ARG WORKSPACE=/app

WORKDIR ${WORKSPACE}

ENV PATH="${PATH}:${WORKSPACE}/bin"

RUN apk --no-cache add ca-certificates
RUN addgroup -g $GID -S $GROUP_NAME \
    && adduser --shell /sbin/nologin --disabled-password \
    -h "${WORKSPACE}" \
    --no-create-home -u $UID -S $USER_NAME -G $GROUP_NAME


COPY --from=builder /magneto.internal/bin/magneto ./bin/magneto
USER "${USER_NAME}"

# Command to run the executable
ENTRYPOINT ["magneto"]
