BIN_DIR=_output/bin

all: controllers scheduler

init:
	mkdir -p ${BIN_DIR}

controllers:
	go build -o ${BIN_DIR}/controllers ./cmd/controllers

scheduler:
	go build -o ${BIN_DIR}/scheduler ./cmd/scheduler

verify: generate-code
	hack/verify-gofmt.sh
	hack/verify-golint.sh
	hack/verify-gencode.sh

generate-code:
	go build -o ${BIN_DIR}/deepcopy-gen ./cmd/deepcopy-gen/
	${BIN_DIR}/deepcopy-gen -i ./pkg/apis/core/v1alpha1/ -O zz_generated.deepcopy

run-test:
	hack/make-rules/test.sh $(WHAT) $(TESTS)

e2e: all
	hack/e2e-cluster.sh
	cd test && go test -v

clean:
	rm -rf _output/
