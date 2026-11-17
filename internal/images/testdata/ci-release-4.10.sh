# Exact function excerpts from stackrox/stackrox@de310ded87bdec2cbe7f2750c6f9625632a39260/scripts/ci/lib.sh
# Source fixture only. Never execute this file.

registry_from_branding() {
    local branding="$1"
    if [[ "$branding" == "STACKROX_BRANDING" ]]; then
        registry="quay.io/stackrox-io"
    elif [[ "$branding" == "RHACS_BRANDING" ]]; then
        registry="quay.io/rhacs-eng"
    else
        die "$branding is not a supported branding"
    fi
    echo "$registry"
}

push_matching_collector_scanner_images() {
    info "Pushing collector & scanner images tagged with main-version to quay.io/rhacs-eng"

    if [[ "$#" -ne 2 ]]; then
        die "missing arg. usage: push_matching_collector_scanner_images <brand> <arch>"
    fi

    local brand="$1"
    local arch="$2"

    local registry
    registry="$(registry_from_branding "$brand")"

    _retag() {
        retry 5 true "$SCRIPTS_ROOT/scripts/ci/pull-retag-push.sh" "$1" "$2"
    }

    if [[ "$arch" != "amd64" ]]; then
        echo "Skipping rebundling for non-amd64 arch"
        exit 0
    fi

    local main_tag
    main_tag="$(make --quiet --no-print-directory tag)"
    local scanner_version
    scanner_version="$(make --quiet --no-print-directory scanner-tag)"
    local collector_version
    collector_version="$(make --quiet --no-print-directory collector-tag)"
    local fact_version
    fact_version="$(make --quiet --no-print-directory fact-tag)"

    registry_rw_login "${registry}"

    _retag "${registry}/scanner:${scanner_version}"    "${registry}/scanner:${main_tag}-${arch}"
    _retag "${registry}/scanner-db:${scanner_version}" "${registry}/scanner-db:${main_tag}-${arch}"
    _retag "${registry}/scanner-slim:${scanner_version}"    "${registry}/scanner-slim:${main_tag}-${arch}"
    _retag "${registry}/scanner-db-slim:${scanner_version}" "${registry}/scanner-db-slim:${main_tag}-${arch}"

    _retag "${registry}/collector:${collector_version}"      "${registry}/collector:${main_tag}-${arch}"

    _retag "${registry}/fact:${fact_version}" "${registry}/fact:${main_tag}-${arch}"
}
