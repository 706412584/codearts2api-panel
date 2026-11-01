#!/usr/bin/env bash
# Offline regression tests. No Docker daemon, registry, credentials or server needed.
# Run in WSL/Linux: bash deploy/tests/deploy_test.sh
# All Docker invocations go to the stub below; all mutable fixtures live in mktemp.
set -Eeuo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)
DEPLOY_SCRIPT="$SCRIPT_DIR/../deploy.sh"
if [[ ! -f "$DEPLOY_SCRIPT" ]]; then
  printf 'Missing deployment script: %s\n' "$DEPLOY_SCRIPT" >&2
  exit 2
fi
for required in bash env mktemp mkdir chmod cp cmp diff flock mkfifo rm; do
  if ! command -v "$required" >/dev/null 2>&1; then
    printf 'Missing test prerequisite: %s\n' "$required" >&2
    exit 2
  fi
done

TEMP_BASE=$(CDPATH= cd -- "${TMPDIR:-/tmp}" && pwd -P)
TEST_ROOT=$(mktemp -d -- "$TEMP_BASE/codearts2api-deploy-tests.XXXXXXXX")
readonly TEMP_BASE TEST_ROOT
cleanup() {
  local result=$?
  trap - EXIT
  # Never recursively delete an unchecked or user-supplied directory.
  if [[ -n "$TEST_ROOT" && -d "$TEST_ROOT" &&
        "$TEST_ROOT" == "$TEMP_BASE"/codearts2api-deploy-tests.* &&
        "$TEST_ROOT" != "$TEMP_BASE" ]]; then
    rm -rf -- "$TEST_ROOT"
  else
    printf 'Refusing to clean unexpected test path: %s\n' "$TEST_ROOT" >&2
    result=1
  fi
  exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

STUB_BIN="$TEST_ROOT/stub bin"
mkdir -p -- "$STUB_BIN"
cat >"$STUB_BIN/docker" <<'DOCKER_STUB'
#!/usr/bin/env bash
set -Eeuo pipefail
# Deliberately no fallback to a real docker binary.
[[ ${DEPLOY_TEST_DOCKER_STUB:-} == 1 && -d ${STUB_ROOT:-} ]] || exit 99
root=$STUB_ROOT
{
  printf 'docker'
  printf ' %q' "$@"
  printf '\n'
} >>"$root/docker.commands"

event() { printf '%s\n' "$1" >>"$root/docker.events"; }
violation() {
  event violation
  printf 'Docker stub contract violation: %s\n' "$*" >&2
  exit 97
}
expected_dir=$(<"$root/deploy-dir")
requested=$(<"$root/requested-ref")

inspect_container() {
  local format= id=
  while (($#)); do
    case "$1" in
      --format|-f) (($# >= 2)) || violation 'missing inspect format'; format=$2; shift 2 ;;
      --format=*) format=${1#*=}; shift ;;
      *) [[ -z "$id" ]] || violation 'multiple container ids'; id=$1; shift ;;
    esac
  done
  [[ "$id" == stub-managed-container ]] || violation 'unexpected container id'
  [[ "$format" == *Config.Image* ]] || violation 'container query must read Config.Image'
  [[ $(<"$root/container-present") == 1 ]] || violation 'inspected a nonexistent container'
  event inspect
  printf '%s\n' "$(<"$root/running-image")"
}

case "${1:-}" in
  inspect)
    shift
    inspect_container "$@"
    ;;
  container)
    [[ ${2:-} == inspect ]] || violation 'unsupported container command'
    shift 2
    inspect_container "$@"
    ;;
  pull)
    shift
    (($# == 1)) || violation 'pull expects exactly one image reference'
    [[ "$1" == "$requested" ]] || violation 'pull changed the requested image reference'
    event pull
    printf '%s\n' "$1" >"$root/pulled-ref"
    [[ ! -e "$root/pull-fail" ]] || exit 31
    # Simulate an operator editing the source after the deployment has started.
    if [[ -e "$root/mutate-compose-on-pull" ]]; then
      source_compose=$(<"$root/input-compose")
      [[ "$source_compose" == "$root"/* ]] || violation 'mutation escaped the fixture'
      cp -- "$root/mutated.compose.yml" "$source_compose"
    fi
    ;;
  image)
    [[ ${2:-} == inspect ]] || violation 'unsupported image command'
    shift 2
    format= reference=
    while (($#)); do
      case "$1" in
        --format|-f) (($# >= 2)) || violation 'missing image format'; format=$2; shift 2 ;;
        --format=*) format=${1#*=}; shift ;;
        *) [[ -z "$reference" ]] || violation 'multiple image references'; reference=$1; shift ;;
      esac
    done
    [[ "$reference" == "$requested" ]] || violation 'image inspect used the wrong reference'
    event image-inspect
    [[ ! -e "$root/image-inspect-fail" ]] || exit 32
    case "$format" in
      '{{index .RepoDigests 0}}')
        first=
        IFS= read -r first <"$root/repo-digests" || :
        printf '%s\n' "$first"
        ;;
      '{{range .RepoDigests}}{{println .}}{{end}}')
        while IFS= read -r digest || [[ -n "$digest" ]]; do
          printf '%s\n' "$digest"
        done <"$root/repo-digests"
        ;;
      *) violation 'unsupported RepoDigests format; synchronize the test stub' ;;
    esac
    ;;
  compose)
    shift
    if [[ ${1:-} == version ]]; then
      event version
      [[ ! -e "$root/version-fail" ]] || exit 33
      printf 'Docker Compose version v2.99.0-test-stub\n'
      exit 0
    fi
    project= project_dir= env_file= compose_file= action=
    while (($#)); do
      case "$1" in
        --project-name|-p) (($# >= 2)) || violation 'missing project name'; project=$2; shift 2 ;;
        --project-name=*) project=${1#*=}; shift ;;
        --project-directory) (($# >= 2)) || violation 'missing project directory'; project_dir=$2; shift 2 ;;
        --project-directory=*) project_dir=${1#*=}; shift ;;
        --env-file) (($# >= 2)) || violation 'missing env file'; env_file=$2; shift 2 ;;
        --env-file=*) env_file=${1#*=}; shift ;;
        --file|-f) (($# >= 2)) || violation 'missing compose file'; compose_file=$2; shift 2 ;;
        --file=*) compose_file=${1#*=}; shift ;;
        config|ps|up|stop|rm) action=$1; shift; break ;;
        down|prune|build) violation 'destructive or build command is forbidden' ;;
        *) violation "unsupported compose option: $1" ;;
      esac
    done
    [[ "$project" == codearts2api-panel ]] || violation 'wrong production project'
    [[ "$project_dir" == "$expected_dir" ]] || violation 'wrong project directory'
    [[ ${DEPLOY_DIR:-} == "$expected_dir" ]] || violation 'DEPLOY_DIR must be exported'
    [[ -n ${IMAGE_REF:-} ]] || violation 'IMAGE_REF must be exported'
    [[ "$env_file" == /dev/null ]] || violation 'must isolate unexpected .env with --env-file /dev/null'
    [[ -f "$compose_file" ]] || violation 'compose --file must name an existing regular file'
    source_compose=$(<"$root/input-compose")
    [[ "$compose_file" != "$source_compose" &&
       "$compose_file" != "$expected_dir/.current-compose.yml" &&
       "$compose_file" != "$expected_dir/.previous-compose.yml" ]] ||
      violation 'compose must use a staged snapshot, not a mutable source/state file'
    event "$action"
    case "$action" in
      config)
        [[ "$*" == --quiet || "$*" == -q ]] || violation 'config validation should not print configuration'
        [[ ! -e "$root/config-fail" ]] || exit 34
        ;;
      ps)
        service=
        while (($#)); do
          case "$1" in
            --all|-a|--quiet|-q) shift ;;
            codearts2api-panel) [[ -z "$service" ]] || violation 'duplicate ps service'; service=$1; shift ;;
            *) violation "unexpected ps argument: $1" ;;
          esac
        done
        [[ "$service" == codearts2api-panel ]] || violation 'ps must target only the managed service'
        if [[ $(<"$root/container-present") == 1 ]]; then
          printf 'stub-managed-container\n'
        fi
        ;;
      up)
        detach=0 no_build=0 pull_policy= wait_ready=0 wait_timeout= service=
        while (($#)); do
          case "$1" in
            --detach|-d) detach=1; shift ;;
            --no-build) no_build=1; shift ;;
            --pull) (($# >= 2)) || violation 'missing pull policy'; pull_policy=$2; shift 2 ;;
            --pull=*) pull_policy=${1#*=}; shift ;;
            --wait) wait_ready=1; shift ;;
            --wait-timeout) (($# >= 2)) || violation 'missing wait timeout'; wait_timeout=$2; shift 2 ;;
            --wait-timeout=*) wait_timeout=${1#*=}; shift ;;
            codearts2api-panel) [[ -z "$service" ]] || violation 'duplicate up service'; service=$1; shift ;;
            *) violation "unexpected up argument: $1" ;;
          esac
        done
        [[ "$detach:$no_build:$pull_policy:$wait_ready:$wait_timeout:$service" == '1:1:never:1:120:codearts2api-panel' ]] ||
          violation 'unsafe up flags or wrong service'
        [[ "$IMAGE_REF" =~ ^ghcr\.io/[a-z0-9._-]+/[a-z0-9._-]+@sha256:[0-9a-f]{64}$ ]] ||
          violation 'up must use an immutable GHCR digest, never a mutable tag'
        count=$(<"$root/up-count")
        count=$((count + 1))
        printf '%s\n' "$count" >"$root/up-count"
        printf '%s\n' "$IMAGE_REF" >"$root/up.$count.image"
        printf '%s\n' "$compose_file" >"$root/up.$count.path"
        cp -- "$compose_file" "$root/up.$count.compose"
        printf '1\n' >"$root/running"
        printf '1\n' >"$root/container-present"
        printf '%s\n' "$IMAGE_REF" >"$root/running-image"
        if [[ -e "$root/up-fail-all" || ( "$count" == 1 && -e "$root/up-fail-first" ) ]]; then
          printf 'test stub: health check failed\n' >&2
          exit 42
        fi
        ;;
      stop)
        [[ "$*" == codearts2api-panel ]] || violation 'stop must target only the failed service'
        [[ ! -e "$root/stop-fail" ]] || exit 43
        printf '0\n' >"$root/running"
        printf '1\n' >"$root/stopped"
        ;;
      rm)
        [[ "$*" == '--force codearts2api-panel' || "$*" == '-f codearts2api-panel' ]] ||
          violation 'rm must target only the failed service, without --volumes'
        [[ -f "$root/stopped" && $(<"$root/running") == 0 ]] || violation 'rm before successful stop'
        [[ ! -e "$root/rm-fail" ]] || exit 44
        printf '0\n' >"$root/container-present"
        : >"$root/running-image"
        ;;
      *) violation 'missing compose action' ;;
    esac
    ;;
  *) violation 'unsupported docker command (real docker is never called)' ;;
esac
DOCKER_STUB
bash -n "$STUB_BIN/docker"
chmod +x -- "$STUB_BIN/docker"

IMAGE_REPO=ghcr.io/example/codearts2api-panel
printf -v OLD_HASH '%064d' 1
printf -v NEW_HASH '%064d' 2
printf -v PREVIOUS_HASH '%064d' 3
OLD_DIGEST="$IMAGE_REPO@sha256:$OLD_HASH"
NEW_DIGEST="$IMAGE_REPO@sha256:$NEW_HASH"
PREVIOUS_DIGEST="$IMAGE_REPO@sha256:$PREVIOUS_HASH"
TAG_REF="$IMAGE_REPO:candidate"
FAKE_CREDENTIAL='test-only-FAKE-credential-not-a-real-secret'
CASE_NUMBER=0
FIXTURE_NUMBER=0
PASSED=0
FAILED=0
CASE_ROOT=
LOCK_PID=

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_equal() {
  [[ "$1" == "$2" ]] || fail "$3 (expected '$2', got '$1')"
}
assert_file_equal() {
  [[ -f "$1" && -f "$2" ]] || fail "$3 (missing regular file)"
  cmp -s -- "$1" "$2" || fail "$3 (bytes differ)"
}
assert_absent() { [[ ! -e "$1" ]] || fail "$2"; }
assert_success() { ((DEPLOY_STATUS == 0)) || fail "expected success, exit=$DEPLOY_STATUS"; }
assert_failure() { ((DEPLOY_STATUS != 0)) || fail 'expected a nonzero deployment exit status'; }
event_count() {
  local wanted=$1 count=0 item
  while IFS= read -r item; do
    if [[ "$item" == "$wanted" ]]; then count=$((count + 1)); fi
  done <"$CASE_ROOT/docker.events"
  printf '%s' "$count"
}
assert_count() { assert_equal "$(event_count "$1")" "$2" "unexpected number of '$1' commands"; }
assert_before() {
  local first=$1 second=$2 found=0 item
  while IFS= read -r item; do
    if [[ "$item" == "$first" ]]; then found=1; fi
    if [[ "$item" == "$second" ]]; then
      ((found == 1)) || fail "'$second' happened before '$first'"
      return 0
    fi
  done <"$CASE_ROOT/docker.events"
  fail "missing '$second' command"
}
assert_no_updates() {
  assert_count pull 0
  assert_count up 0
  assert_count stop 0
  assert_count rm 0
}
assert_no_stub_violations() { assert_count violation 0; }

write_compose() {
  local destination=$1 marker=$2
  # Only inert test data; no real secrets or host/server paths are copied.
  cat >"$destination" <<EOF
# fixture revision: $marker
services:
  codearts2api-panel:
    image: \${IMAGE_REF}
    volumes:
      - \${DEPLOY_DIR}/config.json:/app/config.json
      - \${DEPLOY_DIR}/auths:/app/auths
      - \${DEPLOY_DIR}/data:/app/data
EOF
}

new_fixture() {
  local variant=${1:-normal} item
  FIXTURE_NUMBER=$((FIXTURE_NUMBER + 1))
  CASE_ROOT="$TEST_ROOT/case $CASE_NUMBER fixture $FIXTURE_NUMBER"
  DEPLOY_DIR="$CASE_ROOT/deployment directory with spaces"
  INPUT_COMPOSE="$CASE_ROOT/candidate compose with spaces.yml"
  LOCK_PID=
  OMIT_COMPOSE_ARGUMENT=0
  mkdir -p -- "$CASE_ROOT" "$DEPLOY_DIR" "$CASE_ROOT/home" "$CASE_ROOT/docker config" \
    "$CASE_ROOT/tmp directory" "$CASE_ROOT/persistence-before" "$CASE_ROOT/state-before"
  printf '%s\n' "$DEPLOY_DIR" >"$CASE_ROOT/deploy-dir"
  printf '%s\n' "$INPUT_COMPOSE" >"$CASE_ROOT/input-compose"
  printf '%s\n' "$TAG_REF" >"$CASE_ROOT/requested-ref"
  printf '%s\n' "$NEW_DIGEST" >"$CASE_ROOT/repo-digests"
  printf '0\n' >"$CASE_ROOT/running"
  printf '0\n' >"$CASE_ROOT/container-present"
  printf '0\n' >"$CASE_ROOT/up-count"
  : >"$CASE_ROOT/docker.events"
  : >"$CASE_ROOT/docker.commands"
  case "$variant" in
    missing-config) ;;
    config-directory) mkdir -- "$DEPLOY_DIR/config.json" ;;
    *) printf '{"testCredential":"%s"}\n' "$FAKE_CREDENTIAL" >"$DEPLOY_DIR/config.json" ;;
  esac
  for item in auths data; do
    case "$variant" in
      missing-"$item") ;;
      "$item"-file) printf 'not a directory\n' >"$DEPLOY_DIR/$item" ;;
      *)
        mkdir -- "$DEPLOY_DIR/$item"
        printf 'untouched %s fixture\n' "$item" >"$DEPLOY_DIR/$item/.sentinel"
        printf '%s\n' "$FAKE_CREDENTIAL" >"$DEPLOY_DIR/$item/test-only.json"
        ;;
    esac
  done
  # An accidental Compose .env load would override these exported inputs.
  printf 'IMAGE_REF=docker.io/forbidden/accidental:latest\nDEPLOY_DIR=/do-not-touch-server\n' >"$DEPLOY_DIR/.env"
  case "$variant" in
    missing-compose) ;;
    compose-directory) mkdir -- "$INPUT_COMPOSE" ;;
    *) write_compose "$INPUT_COMPOSE" candidate ;;
  esac
  for item in config.json auths data; do
    if [[ -e "$DEPLOY_DIR/$item" ]]; then
      cp -a -- "$DEPLOY_DIR/$item" "$CASE_ROOT/persistence-before/$item"
    fi
  done
}

seed_old_deployment() {
  local variant=${1:-normal}
  printf '1\n' >"$CASE_ROOT/running"
  printf '1\n' >"$CASE_ROOT/container-present"
  printf '%s\n' "$OLD_DIGEST" >"$CASE_ROOT/running-image"
  printf '%s\n' "$OLD_DIGEST" >"$DEPLOY_DIR/.current-image"
  if [[ "$variant" != no-current-compose ]]; then
    write_compose "$DEPLOY_DIR/.current-compose.yml" old-current
    cp -- "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/old-current.expected.yml"
  fi
  printf '%s\n' "$PREVIOUS_DIGEST" >"$DEPLOY_DIR/.previous-image"
  write_compose "$DEPLOY_DIR/.previous-compose.yml" previous
  cp -- "$DEPLOY_DIR/.previous-compose.yml" "$CASE_ROOT/previous.expected.yml"
}

snapshot_state() {
  local item
  for item in .current-image .current-compose.yml .previous-image .previous-compose.yml; do
    if [[ -e "$DEPLOY_DIR/$item" ]]; then
      cp -- "$DEPLOY_DIR/$item" "$CASE_ROOT/state-before/$item"
    fi
  done
}
assert_state_unchanged() {
  local item
  for item in .current-image .current-compose.yml .previous-image .previous-compose.yml; do
    if [[ -e "$CASE_ROOT/state-before/$item" ]]; then
      assert_file_equal "$DEPLOY_DIR/$item" "$CASE_ROOT/state-before/$item" "state changed: $item"
    else
      assert_absent "$DEPLOY_DIR/$item" "unexpected state was created: $item"
    fi
  done
}
assert_persistence_unchanged() {
  local item output
  for item in config.json auths data; do
    if [[ -e "$CASE_ROOT/persistence-before/$item" ]]; then
      [[ -e "$DEPLOY_DIR/$item" ]] || fail "persistent path disappeared: $item"
      if ! output=$(diff -r -- "$CASE_ROOT/persistence-before/$item" "$DEPLOY_DIR/$item" 2>&1); then
        fail "persistent path changed: $item ($output)"
      fi
    else
      assert_absent "$DEPLOY_DIR/$item" "missing persistent path was unexpectedly created: $item"
    fi
  done
}

run_deploy() {
  local requested=${1-$TAG_REF} compose=${2-$INPUT_COMPOSE} deploy=${3-$DEPLOY_DIR}
  local -a compose_arguments=("$compose")
  if [[ ${OMIT_COMPOSE_ARGUMENT:-0} == 1 ]]; then compose_arguments=(); fi
  printf '%s\n' "$requested" >"$CASE_ROOT/requested-ref"
  printf '%s\n' "$compose" >"$CASE_ROOT/input-compose"
  DEPLOY_STATUS=0
  # env -i also drops BASH_ENV, existing Docker contexts, credentials and server settings.
  # The invalid socket is defense-in-depth; the stub never invokes another docker.
  env -i PATH="$STUB_BIN:$PATH" HOME="$CASE_ROOT/home" LC_ALL=C \
    TMPDIR="$CASE_ROOT/tmp directory" DOCKER_CONFIG="$CASE_ROOT/docker config" \
    DOCKER_HOST="unix://$CASE_ROOT/there-is-no-docker-daemon.sock" \
    DEPLOY_TEST_DOCKER_STUB=1 STUB_ROOT="$CASE_ROOT" \
    bash "$DEPLOY_SCRIPT" "$deploy" "$requested" "${compose_arguments[@]}" \
    >"$CASE_ROOT/deploy.output" 2>&1 || DEPLOY_STATUS=$?
  assert_no_stub_violations
  [[ $(<"$CASE_ROOT/deploy.output") != *"$FAKE_CREDENTIAL"* ]] || fail 'deployment printed a credential fixture'
  local leftover
  for leftover in "$DEPLOY_DIR"/.deploy.*; do
    [[ ! -d "$leftover" ]] || fail 'temporary deployment snapshot directory was not cleaned on exit'
  done
}

test_first_install() {
  new_fixture
  cp -- "$INPUT_COMPOSE" "$CASE_ROOT/candidate.expected.yml"
  run_deploy
  assert_success
  assert_count pull 1
  assert_count image-inspect 1
  assert_count up 1
  assert_count stop 0
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'tag was not pinned before up'
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$NEW_DIGEST" 'wrong current image'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/candidate.expected.yml" 'wrong current compose snapshot'
  assert_absent "$DEPLOY_DIR/.previous-image" 'first install invented a previous image'
  assert_absent "$DEPLOY_DIR/.previous-compose.yml" 'first install invented a previous compose'
  assert_before pull image-inspect
  assert_before image-inspect up
  assert_persistence_unchanged
}

test_default_compose() {
  new_fixture
  local default_compose="$SCRIPT_DIR/../compose.yml"
  [[ -f "$default_compose" ]] || fail 'default production compose is missing'
  cp -- "$default_compose" "$CASE_ROOT/default.expected.yml"
  INPUT_COMPOSE=$default_compose
  OMIT_COMPOSE_ARGUMENT=1
  run_deploy
  assert_success
  assert_count pull 1
  assert_count up 1
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'default-compose deployment did not pin its image'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/default.expected.yml" 'default production compose was not snapshotted'
  assert_persistence_unchanged
}

test_digest_input() {
  new_fixture
  run_deploy "$NEW_DIGEST"
  assert_success
  assert_equal "$(<"$CASE_ROOT/pulled-ref")" "$NEW_DIGEST" 'digest input was changed during pull'
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'digest input was not retained'
  assert_persistence_unchanged
}

test_update_success() {
  new_fixture
  seed_old_deployment
  # Metadata can be stale: only the running container's Config.Image is authoritative.
  printf '%s\n' "$PREVIOUS_DIGEST" >"$DEPLOY_DIR/.current-image"
  cp -- "$INPUT_COMPOSE" "$CASE_ROOT/candidate.expected.yml"
  run_deploy
  assert_success
  assert_count up 1
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$NEW_DIGEST" 'wrong new current image'
  assert_equal "$(<"$DEPLOY_DIR/.previous-image")" "$OLD_DIGEST" 'previous image did not come from actual Config.Image'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/candidate.expected.yml" 'wrong new current compose'
  assert_file_equal "$DEPLOY_DIR/.previous-compose.yml" "$CASE_ROOT/old-current.expected.yml" 'previous compose was not old current'
  assert_before inspect pull
  assert_persistence_unchanged
}

test_pull_failure() {
  new_fixture
  seed_old_deployment
  snapshot_state
  : >"$CASE_ROOT/pull-fail"
  run_deploy
  assert_failure
  assert_count pull 1
  assert_count image-inspect 0
  assert_count up 0
  assert_count stop 0
  assert_equal "$(<"$CASE_ROOT/running-image")" "$OLD_DIGEST" 'pull failure changed the running container'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_health_failure_rollback() {
  new_fixture
  seed_old_deployment
  snapshot_state
  : >"$CASE_ROOT/up-fail-first"
  run_deploy
  assert_failure
  assert_count pull 1
  assert_count up 2
  assert_count stop 0
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'candidate up used the wrong image'
  assert_equal "$(<"$CASE_ROOT/up.2.image")" "$OLD_DIGEST" 'rollback did not use actual old digest'
  assert_file_equal "$CASE_ROOT/up.2.compose" "$CASE_ROOT/old-current.expected.yml" 'rollback did not use old current compose'
  assert_equal "$(<"$CASE_ROOT/running-image")" "$OLD_DIGEST" 'rollback did not restore the old container'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_rollback_failure() {
  new_fixture
  seed_old_deployment
  snapshot_state
  : >"$CASE_ROOT/up-fail-all"
  run_deploy
  assert_failure
  assert_count up 2
  assert_count stop 0
  assert_equal "$(<"$CASE_ROOT/up.2.image")" "$OLD_DIGEST" 'failed rollback attempted the wrong image'
  assert_file_equal "$CASE_ROOT/up.2.compose" "$CASE_ROOT/old-current.expected.yml" 'failed rollback used the wrong compose'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_first_install_failure() {
  local output
  new_fixture
  snapshot_state
  : >"$CASE_ROOT/up-fail-first"
  run_deploy
  assert_failure
  assert_count up 1
  assert_count stop 1
  assert_count rm 1
  assert_equal "$(<"$CASE_ROOT/running")" 0 'failed first-install service was not stopped'
  assert_equal "$(<"$CASE_ROOT/container-present")" 0 'failed first-install container was not removed'
  output=$(<"$CASE_ROOT/deploy.output")
  [[ ! "$output" =~ [Rr]ollback[[:space:]]+(succeeded|successful|complete) &&
     ! "$output" =~ [Rr]olled[[:space:]]+back[[:space:]]+successfully &&
     "$output" != *'回滚成功'* && "$output" != *'已回滚'* ]] ||
    fail 'first-install failure falsely claimed successful rollback'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_first_install_retry() {
  new_fixture
  snapshot_state
  # The first up alone fails; the next invocation should be a fresh install.
  : >"$CASE_ROOT/up-fail-first"
  run_deploy
  assert_failure
  assert_count up 1
  assert_count stop 1
  assert_count rm 1
  assert_state_unchanged
  assert_equal "$(<"$CASE_ROOT/container-present")" 0 'failed container would block a first-install retry'
  run_deploy
  assert_success
  assert_count pull 2
  assert_count up 2
  assert_count inspect 0
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$NEW_DIGEST" 'first-install retry saved the wrong image'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$INPUT_COMPOSE" 'first-install retry saved the wrong compose'
  assert_absent "$DEPLOY_DIR/.previous-image" 'first-install retry invented previous image state'
  assert_absent "$DEPLOY_DIR/.previous-compose.yml" 'first-install retry invented previous compose state'
  assert_persistence_unchanged
}

test_first_install_stop_failure() {
  new_fixture
  snapshot_state
  : >"$CASE_ROOT/up-fail-first"
  : >"$CASE_ROOT/stop-fail"
  run_deploy
  assert_failure
  assert_count up 1
  assert_count stop 1
  assert_count rm 0
  assert_equal "$(<"$CASE_ROOT/container-present")" 1 'rm ran after stop failed'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_invalid_refs() {
  local reference
  local -a invalid=(
    '' 'nginx:latest' 'docker.io/example/panel:latest'
    'GHCR.io/example/panel:latest' 'ghcr.io/Example/panel:latest'
    'ghcr.io/example/Panel:latest' 'ghcr.io/example/panel'
    'ghcr.io/example/panel:' 'ghcr.io/example/panel:bad tag'
    'ghcr.io/example/panel:tag/extra' 'ghcr.io/example/panel:tag;echo unsafe'
    'ghcr.io/example/panel@sha256:1234'
    "ghcr.io/example/panel@sha256:${NEW_HASH}g"
    "ghcr.io/example/panel:tag@sha256:$NEW_HASH"
    'ghcr.io/example/nested/panel:tag' 'https://ghcr.io/example/panel:tag'
    $'ghcr.io/example/panel:tag\n' '--help'
  )
  for reference in "${invalid[@]}"; do
    new_fixture
    snapshot_state
    run_deploy "$reference"
    assert_failure
    assert_no_updates
    assert_state_unchanged
    assert_persistence_unchanged
  done
}

test_missing_or_wrong_paths() {
  local variant
  for variant in missing-config config-directory missing-auths auths-file \
      missing-data data-file missing-compose compose-directory; do
    new_fixture "$variant"
    snapshot_state
    run_deploy
    assert_failure
    assert_no_updates
    assert_state_unchanged
    assert_persistence_unchanged
  done
  new_fixture
  snapshot_state
  run_deploy "$TAG_REF" "$INPUT_COMPOSE" "$DEPLOY_DIR/not-existing"
  assert_failure
  assert_no_updates
  assert_absent "$DEPLOY_DIR/not-existing" 'deployment silently created a missing deploy directory'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_refuse_unmanaged_container() {
  new_fixture
  seed_old_deployment no-current-compose
  snapshot_state
  run_deploy
  assert_failure
  assert_no_updates
  assert_state_unchanged
  assert_persistence_unchanged
}

test_refuse_mutable_old_image() {
  new_fixture
  seed_old_deployment
  printf '%s\n' "$IMAGE_REPO:old-mutable-tag" >"$CASE_ROOT/running-image"
  snapshot_state
  run_deploy
  assert_failure
  assert_no_updates
  assert_state_unchanged
  assert_persistence_unchanged
}

test_manual_previous_compose() {
  new_fixture
  seed_old_deployment
  printf '%s\n' "$PREVIOUS_DIGEST" >"$CASE_ROOT/repo-digests"
  run_deploy "$PREVIOUS_DIGEST" "$DEPLOY_DIR/.previous-compose.yml"
  assert_success
  assert_count up 1
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$PREVIOUS_DIGEST" 'manual rollback deployed the wrong digest'
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$PREVIOUS_DIGEST" 'manual rollback current image is wrong'
  assert_equal "$(<"$DEPLOY_DIR/.previous-image")" "$OLD_DIGEST" 'manual rollback previous image is wrong'
  assert_file_equal "$CASE_ROOT/up.1.compose" "$CASE_ROOT/previous.expected.yml" 'source previous compose was overwritten before up'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/previous.expected.yml" 'manual rollback lost the candidate compose snapshot'
  assert_file_equal "$DEPLOY_DIR/.previous-compose.yml" "$CASE_ROOT/old-current.expected.yml" 'manual rollback did not swap old current into previous'
  assert_persistence_unchanged
}

test_candidate_snapshot() {
  new_fixture
  cp -- "$INPUT_COMPOSE" "$CASE_ROOT/candidate.expected.yml"
  write_compose "$CASE_ROOT/mutated.compose.yml" edited-after-pull
  : >"$CASE_ROOT/mutate-compose-on-pull"
  run_deploy
  assert_success
  assert_file_equal "$INPUT_COMPOSE" "$CASE_ROOT/mutated.compose.yml" 'stub did not simulate source modification'
  assert_file_equal "$CASE_ROOT/up.1.compose" "$CASE_ROOT/candidate.expected.yml" 'up reread the mutable source after pull'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$CASE_ROOT/candidate.expected.yml" 'saved compose was not the deployed snapshot'
  assert_persistence_unchanged
}

test_digest_repository_selection() {
  new_fixture
  printf 'docker.io/example/codearts2api-panel@sha256:%s\nghcr.io/someone/other-image@sha256:%s\n%s\n' \
    "$PREVIOUS_HASH" "$PREVIOUS_HASH" "$NEW_DIGEST" >"$CASE_ROOT/repo-digests"
  run_deploy
  assert_success
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'selected a RepoDigest from another repository'
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$NEW_DIGEST" 'saved the wrong repository digest'
  assert_persistence_unchanged
}

test_explicit_digest_multiple_repo_digests() {
  new_fixture
  seed_old_deployment
  printf '%s\n%s\n' "$OLD_DIGEST" "$NEW_DIGEST" >"$CASE_ROOT/repo-digests"
  run_deploy "$NEW_DIGEST"
  assert_success
  assert_count pull 1
  assert_count image-inspect 1
  assert_count up 1
  assert_equal "$(<"$CASE_ROOT/up.1.image")" "$NEW_DIGEST" 'explicit digest did not select its exact match after an older same-repository digest'
  assert_equal "$(<"$DEPLOY_DIR/.current-image")" "$NEW_DIGEST" 'explicit digest saved the older same-repository digest'
  assert_equal "$(<"$DEPLOY_DIR/.previous-image")" "$OLD_DIGEST" 'explicit digest update did not retain the actual old image'
  assert_file_equal "$DEPLOY_DIR/.current-compose.yml" "$INPUT_COMPOSE" 'explicit digest update saved the wrong candidate compose'
  assert_file_equal "$DEPLOY_DIR/.previous-compose.yml" "$CASE_ROOT/old-current.expected.yml" 'explicit digest update lost the old compose'
  assert_persistence_unchanged
}

test_tag_ambiguous_repo_digests() {
  new_fixture
  seed_old_deployment
  snapshot_state
  printf '%s\n%s\n' "$OLD_DIGEST" "$NEW_DIGEST" >"$CASE_ROOT/repo-digests"
  run_deploy "$TAG_REF"
  assert_failure
  assert_count pull 1
  assert_count image-inspect 1
  assert_count up 0
  assert_count stop 0
  assert_count rm 0
  assert_equal "$(<"$CASE_ROOT/running-image")" "$OLD_DIGEST" 'ambiguous tag digest metadata changed the running container'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_invalid_pulled_digests() {
  local digest
  local -a invalid=(
    "docker.io/example/codearts2api-panel@sha256:$NEW_HASH"
    "ghcr.io/another/codearts2api-panel@sha256:$NEW_HASH"
    "$IMAGE_REPO@sha256:short" "$IMAGE_REPO:not-a-digest" '<no value>' ''
  )
  for digest in "${invalid[@]}"; do
    new_fixture
    seed_old_deployment
    snapshot_state
    printf '%s\n' "$digest" >"$CASE_ROOT/repo-digests"
    run_deploy
    assert_failure
    assert_count pull 1
    assert_count up 0
    assert_count stop 0
    assert_equal "$(<"$CASE_ROOT/running-image")" "$OLD_DIGEST" 'invalid pulled digest changed the old container'
    assert_state_unchanged
    assert_persistence_unchanged
  done
}

test_requested_digest_mismatch() {
  new_fixture
  seed_old_deployment
  snapshot_state
  # Same repository is not enough when the caller requested an exact release.
  printf '%s\n' "$OLD_DIGEST" >"$CASE_ROOT/repo-digests"
  run_deploy "$NEW_DIGEST"
  assert_failure
  assert_count pull 1
  assert_count image-inspect 1
  assert_count up 0
  assert_count stop 0
  assert_equal "$(<"$CASE_ROOT/running-image")" "$OLD_DIGEST" 'mismatched release metadata changed the running container'
  assert_state_unchanged
  assert_persistence_unchanged
}

test_compose_preflight_failure() {
  local marker
  for marker in version-fail config-fail; do
    new_fixture
    seed_old_deployment
    snapshot_state
    : >"$CASE_ROOT/$marker"
    run_deploy
    assert_failure
    assert_no_updates
    assert_state_unchanged
    assert_persistence_unchanged
  done
}

test_image_inspect_failure() {
  new_fixture
  seed_old_deployment
  snapshot_state
  : >"$CASE_ROOT/image-inspect-fail"
  run_deploy
  assert_failure
  assert_count pull 1
  assert_count up 0
  assert_count stop 0
  assert_state_unchanged
  assert_persistence_unchanged
}

test_concurrent_lock() {
  local ready_fd release_fd reply lock_result
  new_fixture
  seed_old_deployment
  snapshot_state
  mkfifo -- "$CASE_ROOT/lock-ready" "$CASE_ROOT/lock-release"
  # Open both ends before starting the helper. Blocking reads provide a handshake;
  # no sleep, polling, Docker daemon or race-prone timing assumptions are needed.
  exec {ready_fd}<>"$CASE_ROOT/lock-ready"
  exec {release_fd}<>"$CASE_ROOT/lock-release"
  (
    exec 9>"$DEPLOY_DIR/.deploy.lock"
    flock -n 9 || exit 51
    printf 'locked\n' >&"$ready_fd"
    IFS= read -r -t 15 reply <&"$release_fd"
  ) &
  LOCK_PID=$!
  IFS= read -r -t 10 reply <&"$ready_fd" || fail 'lock helper did not report readiness'
  [[ "$reply" == locked ]] || fail 'unexpected lock-helper response'
  run_deploy
  printf 'release\n' >&"$release_fd"
  lock_result=0
  wait "$LOCK_PID" || lock_result=$?
  LOCK_PID=
  exec {ready_fd}>&-
  exec {release_fd}>&-
  assert_equal "$lock_result" 0 'lock helper failed'
  assert_failure
  assert_no_updates
  assert_state_unchanged
  assert_persistence_unchanged
}

case_exit() {
  local result=$1 file line
  trap - EXIT
  if [[ -n ${LOCK_PID:-} ]]; then
    kill "$LOCK_PID" 2>/dev/null || :
    wait "$LOCK_PID" 2>/dev/null || :
  fi
  if ((result != 0)) && [[ -n ${CASE_ROOT:-} && -d "$CASE_ROOT" ]]; then
    for file in deploy.output docker.commands; do
      if [[ -f "$CASE_ROOT/$file" ]]; then
        printf '\n--- %s (isolated fake data only) ---\n' "$file" >&2
        while IFS= read -r line || [[ -n "$line" ]]; do printf '%s\n' "$line" >&2; done <"$CASE_ROOT/$file"
      fi
    done
  fi
  exit "$result"
}

run_case() {
  local label=$1 function_name=$2 result
  CASE_NUMBER=$((CASE_NUMBER + 1))
  printf '[%02d] %s ... ' "$CASE_NUMBER" "$label"
  # Do not put the subshell inside `if`: that would disable errexit throughout
  # every assertion and could turn a broken regression test into a false pass.
  set +e
  (
    set -Eeuo pipefail
    CASE_ROOT=
    LOCK_PID=
    trap 'case_exit "$?"' EXIT
    "$function_name"
  )
  result=$?
  set -e
  if ((result == 0)); then
    PASSED=$((PASSED + 1))
    printf 'ok\n'
  else
    FAILED=$((FAILED + 1))
    printf 'FAILED\n'
  fi
}

printf 'Offline deploy regression suite (stub Docker; temporary fixtures only)\n'
run_case 'first install pins the pulled digest and preserves persistent paths' test_first_install
run_case 'optional compose argument uses a staged default production compose' test_default_compose
run_case 'explicit immutable digest input succeeds' test_digest_input
run_case 'update saves the actual running image and old compose' test_update_success
run_case 'pull failure leaves the old container and state untouched' test_pull_failure
run_case 'health failure rolls back the old digest and compose, but exits nonzero' test_health_failure_rollback
run_case 'rollback failure still exits nonzero without publishing new state' test_rollback_failure
run_case 'failed first install stops its service without claiming rollback' test_first_install_failure
run_case 'first install can retry after failed-container cleanup' test_first_install_retry
run_case 'failed first-install stop never removes a still-running container' test_first_install_stop_failure
run_case 'invalid image references never pull or update' test_invalid_refs
run_case 'missing or wrongly typed required paths never pull or update' test_missing_or_wrong_paths
run_case 'existing container without current compose is not adopted' test_refuse_unmanaged_container
run_case 'existing mutable container image is not adopted' test_refuse_mutable_old_image
run_case 'manual previous-compose deployment safely swaps compose snapshots' test_manual_previous_compose
run_case 'source compose edits after pull cannot change the staged candidate' test_candidate_snapshot
run_case 'RepoDigest selection stays in the requested GHCR repository' test_digest_repository_selection
run_case 'explicit digest selects its exact match after an older same-repository digest' test_explicit_digest_multiple_repo_digests
run_case 'tag with different same-repository digests refuses updates and preserves state' test_tag_ambiguous_repo_digests
run_case 'invalid pulled digest metadata never reaches up' test_invalid_pulled_digests
run_case 'requested immutable release rejects a different resolved digest' test_requested_digest_mismatch
run_case 'Compose version/config preflight failure never pulls or updates' test_compose_preflight_failure
run_case 'image inspect failure leaves state unchanged' test_image_inspect_failure
run_case 'held deployment lock rejects a concurrent invocation' test_concurrent_lock
printf '\nResult: %d passed, %d failed\n' "$PASSED" "$FAILED"
((FAILED == 0))
