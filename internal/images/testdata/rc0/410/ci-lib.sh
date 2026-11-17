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

push_main_image_set() {
    info "Pushing main, roxctl and central-db images"

    if [[ "$#" -ne 3 ]]; then
        die "missing arg. usage: push_main_image_set <push_context> <brand> <arch>"
    fi

    local push_context="$1"
    local brand="$2"
    local arch="$3"

    local main_image_set=("main" "roxctl" "central-db")

    _push_main_image_set() {
        local registry="$1"
        local tag="$2"

        for image in "${main_image_set[@]}"; do
            retry 5 true \
              docker push "${registry}/${image}:${tag}" | cat
        done
    }

    _tag_main_image_set() {
        local local_tag="$1"
        local registry="$2"
        local remote_tag="$3"

        for image in "${main_image_set[@]}"; do
            docker tag "stackrox/${image}:${local_tag}" "${registry}/${image}:${remote_tag}"
        done
    }

    local registry
    registry="$(registry_from_branding "$brand")"

    local tag
    tag="$(make --quiet --no-print-directory tag)"

    registry_rw_login "$registry"

    _tag_main_image_set "$tag" "$registry" "$tag-$arch"
    _push_main_image_set "$registry" "$tag-$arch"

    if [[ "$push_context" == "merge-to-master" ]]; then
        _tag_main_image_set "$tag" "$registry" "latest-${arch}"
        _push_main_image_set "$registry" "latest-${arch}"
    fi
}

push_scanner_image_set() {
    info "Pushing scanner-v4 and scanner-v4-db images"

    if [[ "$#" -ne 2 ]]; then
        die "missing arg. usage: push_scanner_image_set <registry> <arch>"
    fi

    local registry="$1"
    local arch="$2"

    local scanner_image_set=("scanner-v4" "scanner-v4-db")

    _push_scanner_image_set() {
        local registry="$1"
        local tag="$2"

        for image in "${scanner_image_set[@]}"; do
            retry 5 true \
              docker push "${registry}/${image}:${tag}" | cat
        done
    }

    _tag_scanner_image_set() {
        local local_tag="$1"
        local registry="$2"
        local remote_tag="$3"

        for image in "${scanner_image_set[@]}"; do
            docker tag "stackrox/${image}:${local_tag}" "${registry}/${image}:${remote_tag}"
        done
    }

    local tag
    tag="$(make --quiet --no-print-directory -C scanner tag)"

    registry_rw_login "$registry"

    _tag_scanner_image_set "$tag" "$registry" "$tag-$arch"
    _push_scanner_image_set "$registry" "$tag-$arch"
}

push_image_manifest_lists() {
    info "Pushing main, roxctl and central-db images as manifest lists"

    if [[ "$#" -ne 3 ]]; then
        die "missing arg. usage: push_image_manifest_lists <push_context> <brand> <architectures (CSV)>"
    fi

    local push_context="$1"
    local brand="$2"
    local architectures="$3"

    local main_image_set=("main" "roxctl" "central-db")

    local registry
    registry="$(registry_from_branding "$brand")"

    local tag
    tag="$(make --quiet --no-print-directory tag)"

    registry_rw_login "$registry"
    for image in "${main_image_set[@]}"; do
        retry 5 true \
          "$SCRIPTS_ROOT/scripts/ci/push-as-multiarch-manifest-list.sh" "${registry}/${image}:${tag}" "$architectures" | cat
        if [[ "$push_context" == "merge-to-master" ]]; then
            retry 5 true \
              "$SCRIPTS_ROOT/scripts/ci/push-as-multiarch-manifest-list.sh" "${registry}/${image}:latest" "$architectures" | cat
        fi
    done

    # Push manifest lists for scanner, fact and collector for amd64 only
    local amd64_image_set=("scanner" "scanner-db" "scanner-slim" "scanner-db-slim" "collector" "fact")
    for image in "${amd64_image_set[@]}"; do
        retry 5 true \
          "$SCRIPTS_ROOT/scripts/ci/push-as-multiarch-manifest-list.sh" "${registry}/${image}:${tag}" "amd64" | cat
    done
}

push_scanner_image_manifest_lists() {
    info "Pushing scanner-v4 and scanner-v4-db images as manifest lists"

    if [[ "$#" -ne 2 ]]; then
        die "missing arg. usage: push_scanner_image_manifest_lists <registry> <architectures (CSV)>"
    fi

    local registry="$1"
    local architectures="$2"
    local scanner_image_set=("scanner-v4" "scanner-v4-db")

    local tag
    tag="$(make --quiet --no-print-directory -C scanner tag)"
    registry_rw_login "$registry"
    for image in "${scanner_image_set[@]}"; do
        retry 5 true \
          "$SCRIPTS_ROOT/scripts/ci/push-as-multiarch-manifest-list.sh" "${registry}/${image}:${tag}" "$architectures" | cat
    done
}
