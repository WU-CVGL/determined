#!/usr/bin/env bash
# Release checks of the agent's cgo build (NVML support for the GPU topology).
#
#   tools/fork/check-agent-build.sh binary AGENT_BINARY MASTER_BINARY BASE_IMAGE
#       1. go version -m: the agent was built with CGO_ENABLED=1 (a stub build has no NVML
#          support) and the master with CGO_ENABLED=0;
#       2. readelf -d: the agent needs only glibc libraries;
#       3. objdump -T: the agent's highest GLIBC_ symbol version is not above the glibc of
#          BASE_IMAGE (ldd --version in the image).
#   tools/fork/check-agent-build.sh image AGENT_IMAGE
#       4. determined-agent version runs in the image, and determined-agent gpu-topology exits 0
#          with nvml_init ERROR_LIBRARY_NOT_FOUND (12): the cgo wrapper loads, and its
#          dlopen-failure path works, in the real image on a host without NVIDIA GPUs. A stub
#          build prints NOT_BUILT and fails.
#
# The checks use go, readelf, objdump, docker and jq; they install nothing.
set -euo pipefail

die() {
    echo "error: $*" >&2
    exit 1
}

require() {
    local tool
    for tool in "$@"; do
        command -v "${tool}" >/dev/null 2>&1 || die "${tool} was not found"
    done
}

# Libraries of glibc that a cgo binary may need.
glibc_library() {
    case $1 in
        libc.so.6 | libdl.so.2 | libpthread.so.0 | libm.so.6 | librt.so.1 | libresolv.so.2) return 0 ;;
        ld-linux-x86-64.so.2 | ld-linux-aarch64.so.1) return 0 ;;
        *) return 1 ;;
    esac
}

check_binary() {
    local agent=$1 master=$2 base_image=$3
    require go readelf objdump docker
    [[ -f ${agent} ]] || die "no agent binary at ${agent}"
    [[ -f ${master} ]] || die "no master binary at ${master}"

    go version -m "${agent}" | grep -Eq '^[[:space:]]+build[[:space:]]+CGO_ENABLED=1$' \
        || die "${agent} was not built with CGO_ENABLED=1: it would have no NVML support"
    go version -m "${master}" | grep -Eq '^[[:space:]]+build[[:space:]]+CGO_ENABLED=0$' \
        || die "${master} was not built with CGO_ENABLED=0"

    local -a needed
    mapfile -t needed < <(readelf -d "${agent}" | sed -nE 's/.*\(NEEDED\).*\[([^]]+)\].*/\1/p')
    ((${#needed[@]} > 0)) || die "${agent} needs no shared library: it is not a cgo build"
    local lib
    for lib in "${needed[@]}"; do
        glibc_library "${lib}" || die "${agent} needs ${lib}, which is not part of glibc"
    done

    local highest base
    highest=$(objdump -T "${agent}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sed 's/^GLIBC_//' \
        | sort -uV | tail -n 1)
    [[ -n ${highest} ]] || die "${agent} references no GLIBC_ symbol version"
    base=$(docker run --rm --entrypoint ldd "${base_image}" --version | head -n 1 \
        | grep -oE '[0-9]+\.[0-9]+$') || true
    [[ -n ${base} ]] || die "could not read the glibc version of ${base_image}"
    [[ $(printf '%s\n%s\n' "${highest}" "${base}" | sort -V | tail -n 1) == "${base}" ]] \
        || die "${agent} needs GLIBC_${highest}, but ${base_image} has glibc ${base}"

    echo "agent binary: CGO_ENABLED=1, needs ${needed[*]}, highest GLIBC_${highest}" \
        "<= glibc ${base} of ${base_image}; master binary: CGO_ENABLED=0"
}

check_image() {
    local image=$1 out
    require docker jq
    docker run --rm --entrypoint /usr/bin/determined-agent "${image}" version >/dev/null \
        || die "determined-agent version failed in ${image}"
    out=$(docker run --rm --entrypoint /usr/bin/determined-agent "${image}" gpu-topology) \
        || die "determined-agent gpu-topology did not exit 0 in ${image}"
    jq -e '.nvml_init == "ERROR_LIBRARY_NOT_FOUND" and .nvml_init_code == 12' \
        <<<"${out}" >/dev/null || die "determined-agent gpu-topology in ${image} reported" \
        "$(jq -c '{nvml_init, nvml_init_code}' <<<"${out}" 2>/dev/null || echo "${out}")," \
        'expected nvml_init "ERROR_LIBRARY_NOT_FOUND" and nvml_init_code 12'
    echo "agent image: determined-agent gpu-topology reports ERROR_LIBRARY_NOT_FOUND (12)" \
        "without NVIDIA GPUs"
}

case ${1:-} in
    binary)
        (($# == 4)) || die "usage: $0 binary AGENT_BINARY MASTER_BINARY BASE_IMAGE"
        check_binary "$2" "$3" "$4"
        ;;
    image)
        (($# == 2)) || die "usage: $0 image AGENT_IMAGE"
        check_image "$2"
        ;;
    *)
        die "usage: $0 binary AGENT_BINARY MASTER_BINARY BASE_IMAGE | image AGENT_IMAGE"
        ;;
esac
