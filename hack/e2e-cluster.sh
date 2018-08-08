#!/bin/bash

./hack/dind-cluster-v1.11.sh up

kc=$(which kubectl)

if [ -z ${kc} ]; then
 	curl -LO https://storage.googleapis.com/kubernetes-release/release/$(curl -s https://storage.googleapis.com/kubernetes-release/release/stable.txt)/bin/linux/amd64/kubectl

 	chmod +x ./kubectl

	kc=./kubectl
fi

${kc} config set-cluster dind --server=http://localhost:8080 --insecure-skip-tls-verify
${kc} config set-context dind --cluster=dind --namespace=default
${kc} config set current-context dind

nohup _output/bin/controllers --master localhost:8080 --logtostderr --v 3 > controller.log 2>&1 &
nohup _output/bin/scheduler --master localhost:8080 --logtostderr --v 3 > scheduler.log 2>&1 &

