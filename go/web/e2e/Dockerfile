# e2e toolchain image — THE canonical render environment for @visual baselines
# (design 06 §4.4): browsers, freetype/fontconfig, system fonts AND the package
# manager pinned as one unit. The app ships pure system-font stacks (S11), so
# every pixel depends on the render host's font library — only this pinned
# image makes baselines reproducible.
#
# Built exclusively via e2e-visual.sh (locally) and the PV8 ci.yml web job —
# both read the digest pins from e2e/toolchain.lock (single source, E4).
# Building without the build-args fails loudly on purpose: there are no
# defaults, the lock file is the only place a base ref may live.
ARG PLAYWRIGHT_IMAGE
ARG BUN_IMAGE

FROM ${BUN_IMAGE} AS bun

FROM ${PLAYWRIGHT_IMAGE}
# glibc bun binary (deliberately NOT the -alpine/musl variant) into the noble
# playwright base — the same bun install/run path the release build uses
# (go/Dockerfile stage 0): bun.lock stays the only dependency truth (S4).
COPY --from=bun /usr/local/bin/bun /usr/local/bin/bun
# Deterministic clock zone for toLocale* renders; the browsers path is already
# set by the base image — pinned here explicitly so a base-image change cannot
# silently move it.
ENV TZ=UTC \
    PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
