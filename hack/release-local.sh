#!/bin/bash
set -euo pipefail

REPO="${REPO:-}"
MANIFESTS="${MANIFESTS:-}"
MANIFESTS=/tmp/manifests
mkdir -p $MANIFESTS

if [ -z "$REPO" ]; then echo "REPO is required"; exit 1; fi
if [ -z "$MANIFESTS" ]; then echo "MANIFESTS is required"; exit 1; fi

TEMP_COMMIT="false"
test -z "$(git status --porcelain)" || TEMP_COMMIT="true"

if [[ "${TEMP_COMMIT}" == "true" ]]; then
  git add .
  git commit -m "Temporary" || true
fi

REV=$(git rev-parse --short HEAD)
TAG="${TAG:-$REV}"

#export REGISTRY_AUTH_FILE=/home/aim/.secrets/pull-secret.json

if [[ -z "${DOCKER+1}" ]] && command -v buildah >& /dev/null; then
  podman build -t $REPO:$TAG .
  podman push $REPO:$TAG
else
  podman build -t $REPO:$TAG .
  podman push $REPO:$TAG
fi

if [[ "${TEMP_COMMIT}" == "true" ]]; then
  git reset --soft HEAD~1
fi

cp -R manifests/* $MANIFESTS
cat manifests/02-deployment.yaml | sed "s~openshift/origin-cluster-ingress-operator:latest~$REPO:$TAG~" > "$MANIFESTS/02-deployment.yaml"
# To simulate CVO, ClusterOperator resource need to be created by the operator.
rm $MANIFESTS/03-cluster-operator.yaml

echo "Pushed $REPO:$TAG"
echo "Install manifests using:"
echo ""
echo "oc apply -f $MANIFESTS"
echo ""
echo "Alternatively, rollout just a new operator deployment with:"
echo ""
echo "oc apply -f $MANIFESTS/02-deployment.yaml"
