#!/usr/bin/env bash
set -o errtrace -o pipefail -o noclobber -o errexit -o nounset

APP="check-coverage"

exit_error() {
  echo "${APP} error: ${1}" >&2
  exit "${2:-1}"
}


PROFILE=${1:-}
[ -z "${PROFILE}" ] && exit_error "path to profile file is required"

THRESHOLD=${2:-}
[ -z "${THRESHOLD}" ] && exit_error "coverage threshold is not set"

COVERAGE=$(go tool cover -func="$PROFILE" | grep '^total:' | awk '{print substr($3, 1, length($3) - 1)}')
echo "$COVERAGE $THRESHOLD" | LC_ALL=C awk '{if (!($1 >= $2)) { print "coverage: " $1 "%" ", expected threshold: " $2 "%"; exit 1 } }'
