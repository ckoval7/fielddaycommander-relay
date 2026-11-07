#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
# SPDX-License-Identifier: GPL-3.0-or-later

# Scans this checkout (HEAD plus uncommitted changes) on a local SonarQube server,
# using SonarCloud's own Sonar way comprehensive profiles. The community image's
# default profiles are far looser, so a scan refuses to run without the imported ones.
#
#   scripts/sonar-local.sh up     start the server and import the profiles
#   scripts/sonar-local.sh scan   copy the repo, scan it, print issues and hotspots
#   scripts/sonar-local.sh down   remove the server and everything cached
#
# The container and its cache are throw-away: nothing in them is kept or needed later,
# so they're safe to remove after use, and "up" replaces any left behind.
#
# The SonarCloud token comes from SONAR_TOKEN, or else the app repo's .env beside this
# checkout. It only goes to SonarCloud, and is never printed or saved.
set -euo pipefail

NAME=fdc-relay-sonar
PORT=9012
URL="http://localhost:${PORT}"
PROJECT=fdc-relay
CLOUD_API="https://sonarcloud.io/api"
CLOUD_ORG=ckoval7
CLOUD_PROJECT=ckoval7_fielddaycommander-relay
# Under ~/.cache because Docker on WSL can't see /tmp.
STATE_DIR="${HOME}/.cache/fdc-relay-sonar"
STATE_FILE="${STATE_DIR}/state.env"
SRC_DIR="${STATE_DIR}/src"
REPO="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"

api() { curl -fsS -u "admin:${SONAR_ADMIN_PASSWORD}" "$@"; }

cloud_token() {
    local env_file="${REPO}/../fielddaycommander/.env" token="${SONAR_TOKEN:-}"
    if [[ -z "${token}" && -f "${env_file}" ]]; then
        token="$(grep -E '^SONAR_TOKEN=' "${env_file}" | cut -d= -f2- | tr -d '"' || true)"
    fi
    [[ -n "${token}" ]] || { echo "Set SONAR_TOKEN, or put it in ${env_file}" >&2; exit 1; }
    echo "${token}"
}

wait_for_up() {
    printf 'Waiting for SonarQube'
    for _ in $(seq 1 120); do
        if [[ "$(curl -fsS "${URL}/api/system/status" 2>/dev/null | jq -r .status 2>/dev/null)" = "UP" ]]; then
            echo ' up'
            return 0
        fi
        printf '.'
        sleep 3
    done
    echo ' timed out' >&2
    exit 1
}

local_rule_count() {
    api "${URL}/api/qualityprofiles/search?language=$1" \
        | jq '[.profiles[] | select(.name | startswith("SonarCloud"))][0].activeRuleCount // 0'
}

# Lists only languages where the local server runs fewer rules than SonarCloud, since
# a clean scan says nothing about the others.
import_cloud_profiles() {
    local token local_langs lang name cloud_rules profile local_rules skipped=()
    token="$(cloud_token)"
    local_langs="$(api "${URL}/api/languages/list?ps=0" | jq -r '.languages[].key')"
    mkdir -p "${STATE_DIR}/profiles"
    echo "Importing SonarCloud's profiles. Rules the local server won't run:"
    while IFS=$'\t' read -r lang name cloud_rules; do
        if ! grep -qx "${lang}" <<<"${local_langs}"; then
            skipped+=("${lang}")
            continue
        fi
        [[ "${name}" = "Sonar way comprehensive" ]] \
            || echo "  WARNING: SonarCloud uses '${name}' for ${lang}, not 'Sonar way comprehensive'" >&2
        profile="${STATE_DIR}/profiles/${lang}.xml"
        curl -fsS -u "${token}:" -G "${CLOUD_API}/qualityprofiles/backup" \
            --data-urlencode "language=${lang}" --data-urlencode "qualityProfile=${name}" \
            --data-urlencode "organization=${CLOUD_ORG}" -o "${profile}"
        sed -i "s#<name>${name}</name>#<name>SonarCloud ${lang}</name>#" "${profile}"
        api -X POST -F "backup=@${profile}" "${URL}/api/qualityprofiles/restore" >/dev/null
        api -X POST "${URL}/api/qualityprofiles/set_default" \
            --data-urlencode "language=${lang}" --data-urlencode "qualityProfile=SonarCloud ${lang}"
        local_rules="$(local_rule_count "${lang}")"
        if [[ "${local_rules}" -lt "${cloud_rules}" ]]; then
            printf '  %-22s %s of %s active\n' "${lang}" "${local_rules}" "${cloud_rules}"
        fi
    done < <(curl -fsS -u "${token}:" "${CLOUD_API}/qualityprofiles/search?project=${CLOUD_PROJECT}&organization=${CLOUD_ORG}" \
        | jq -r '.profiles[] | [.language, .name, .activeRuleCount] | @tsv')
    echo "No local analyzer: ${skipped[*]:-none}"
}

cmd_up() {
    mkdir -p "${STATE_DIR}"
    chmod 700 "${STATE_DIR}"
    if [[ -f "${STATE_FILE}" ]] && docker ps --format '{{.Names}}' | grep -qx "${NAME}"; then
        echo "Already running at ${URL}"
        return 0
    fi
    docker rm -f "${NAME}" >/dev/null 2>&1 || true
    rm -f "${STATE_FILE}"
    docker run -d --name "${NAME}" -p "127.0.0.1:${PORT}:9000" sonarqube:community >/dev/null
    wait_for_up

    SONAR_ADMIN_PASSWORD="Fdc-$(openssl rand -hex 10)-Aa1!"
    curl -fsS -u admin:admin -X POST "${URL}/api/users/change_password" \
        --data-urlencode login=admin --data-urlencode previousPassword=admin \
        --data-urlencode "password=${SONAR_ADMIN_PASSWORD}"
    api -X POST "${URL}/api/projects/create" -d "project=${PROJECT}" -d "name=${PROJECT}" >/dev/null
    SONAR_SCAN_TOKEN="$(api -X POST "${URL}/api/user_tokens/generate" -d "name=scan-$(date +%s)" | jq -r .token)"

    import_cloud_profiles

    printf 'SONAR_ADMIN_PASSWORD=%q\nSONAR_SCAN_TOKEN=%q\n' "${SONAR_ADMIN_PASSWORD}" "${SONAR_SCAN_TOKEN}" > "${STATE_FILE}"
    chmod 600 "${STATE_FILE}"
    echo "SonarQube ready at ${URL}"
}

copy_source() {
    rm -rf "${SRC_DIR}"
    mkdir -p "${SRC_DIR}"
    git -C "${REPO}" archive HEAD | tar -x -C "${SRC_DIR}"
    # Overlay uncommitted and untracked (non-ignored) files so the scan sees them.
    while IFS= read -r -d '' file; do
        if [[ -f "${REPO}/${file}" ]]; then
            mkdir -p "${SRC_DIR}/$(dirname "${file}")"
            cp "${REPO}/${file}" "${SRC_DIR}/${file}"
        else
            rm -f "${SRC_DIR}/${file}"
        fi
    done < <(git -C "${REPO}" ls-files -z --modified --others --exclude-standard)
}

report() {
    local page=1 total=1 issues='[]' batch hotspots
    while [[ $(((page - 1) * 500)) -lt "${total}" ]]; do
        batch="$(api "${URL}/api/issues/search?componentKeys=${PROJECT}&issueStatuses=OPEN,CONFIRMED&ps=500&p=${page}")"
        total="$(jq .total <<<"${batch}")"
        issues="$(jq -s '.[0] + .[1].issues' <(echo "${issues}") <(echo "${batch}"))"
        page=$((page + 1))
    done
    hotspots="$(api "${URL}/api/hotspots/search?project=${PROJECT}&status=TO_REVIEW&ps=500" | jq .hotspots)"

    echo
    echo "== Issues: $(jq length <<<"${issues}") open =="
    jq -r '.[] | [(.impacts[0].severity // .severity), .type, .rule, ((.component | sub("^[^:]*:"; "")) + ":" + ((.line // 0) | tostring)), .message] | @tsv' <<<"${issues}" | sort | column -t -s $'\t' || true
    echo
    echo "== Security hotspots: $(jq length <<<"${hotspots}") to review =="
    jq -r '.[] | [.vulnerabilityProbability, .ruleKey, ((.component | sub("^[^:]*:"; "")) + ":" + ((.line // 0) | tostring)), .message] | @tsv' <<<"${hotspots}" | column -t -s $'\t' || true
    echo
    echo "== Quality gate: $(api "${URL}/api/qualitygates/project_status?projectKey=${PROJECT}" | jq -r .projectStatus.status) =="
}

cmd_scan() {
    [[ -f "${STATE_FILE}" ]] || { echo "Run '$0 up' first" >&2; exit 1; }
    # shellcheck disable=SC1090
    source "${STATE_FILE}"
    if [[ "$(local_rule_count go)" = "0" ]]; then
        echo "SonarCloud's profiles aren't imported. Run '$0 down', then '$0 up'." >&2
        exit 1
    fi
    copy_source
    # Scope comes from sonar-project.properties, as in CI. Coverage is off because the
    # local scan doesn't run the tests.
    docker run --rm --network host -u "$(id -u):$(id -g)" \
        -v "${SRC_DIR}:/usr/src" \
        -e SONAR_HOST_URL="${URL}" -e SONAR_TOKEN="${SONAR_SCAN_TOKEN}" \
        sonarsource/sonar-scanner-cli \
        -Dsonar.projectKey="${PROJECT}" \
        -Dsonar.organization= \
        -Dsonar.go.coverage.reportPaths= \
        -Dsonar.coverage.exclusions='**/*' \
        -Dsonar.scm.disabled=true \
        -Dsonar.qualitygate.wait=true \
        -Dsonar.qualitygate.timeout=500 \
        > "${STATE_DIR}/scanner.log" 2>&1 || {
            if ! grep -q 'QUALITY GATE STATUS' "${STATE_DIR}/scanner.log"; then
                tail -30 "${STATE_DIR}/scanner.log" >&2
                echo "The scan failed. Full log: ${STATE_DIR}/scanner.log" >&2
                exit 1
            fi
        }
    report
}

cmd_down() {
    docker rm -f "${NAME}" >/dev/null 2>&1 || true
    rm -rf "${STATE_DIR}"
    echo "Removed ${NAME} and ${STATE_DIR}"
}

case "${1:-}" in
    up) cmd_up ;;
    scan) cmd_scan ;;
    down) cmd_down ;;
    *) echo "Usage: $0 {up|scan|down}" >&2; exit 2 ;;
esac
