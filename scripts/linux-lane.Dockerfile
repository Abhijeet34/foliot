# The image scripts/linux-lane.sh runs this repository's suite in. Its one job is to answer, on
# a macOS workstation, the question CI answers on ubuntu-24.04: does this change pass on Linux.
# So it is built to match that runner rather than to be small, because a lane that differs from
# CI reports a red CI would not, and a lane that cries wolf is a lane nobody runs.
#
# Each line below is a red that was the image's fault, measured 2026-09-16 against this tree at
# a commit CI had already passed:
#   - the Go toolchain is copied from the golang image the go.mod line names, so the lane can
#     never test under a toolchain the project does not declare;
#   - git comes from ppa:git-core, as it does on the GitHub runner: src/core/bench calls
#     `git clone --revision=`, which needs git 2.49, and Ubuntu 24.04 ships 2.43;
#   - node is on PATH because every pinned-clock control in src/core/bench needs it there;
#   - gcc and libc6-dev are present so cgo is available, as it is on the runner.
# The script passes --user, because root ignores a read-only mode bit and the hostile-root
# cases in src/core/log then pass without proving anything.
ARG GO_VERSION
FROM golang:${GO_VERSION} AS toolchain

FROM ubuntu:24.04
COPY --from=toolchain /usr/local/go /usr/local/go
RUN apt-get update -qq \
  && apt-get install -y -qq --no-install-recommends ca-certificates software-properties-common \
  && add-apt-repository -y ppa:git-core/ppa \
  && apt-get update -qq \
  && apt-get install -y -qq --no-install-recommends git nodejs gcc libc6-dev \
  && rm -rf /var/lib/apt/lists/*
ENV PATH=/usr/local/go/bin:$PATH
