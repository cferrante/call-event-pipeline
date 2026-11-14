# The on-demand AWS demo environment (docs/design.md, section 13). Each target needs a
# signed-in AWS profile: aws sso login --profile <PROJECT_PROFILE in infra/bootstrap/aws/config.env>
.PHONY: demo-up demo-smoke demo-spike demo-status demo-down demo-check demo-test

demo-up:    ## create the environment, ship the code, smoke-test it (about $0.23/hour until demo-down)
	./infra/demo/up.sh
demo-smoke: ## re-run the end-to-end smoke test
	./infra/demo/smoke.sh
demo-spike: ## a webhook spike from the load generator, then checks nothing was lost (RATE, HOLD)
	./infra/demo/spike.sh
demo-status: ## forward the status page and alert inbox to localhost:18090 / :18025 (Ctrl-C stops)
	./infra/demo/status.sh
demo-down:  ## destroy everything and confirm nothing is left
	./infra/demo/down.sh
demo-check: ## read-only: list anything still left over
	./infra/demo/check.sh
demo-test:  ## offline tests of the plan check and check.sh; nothing reaches AWS
	go test ./infra/demo/...
	./infra/demo/check_test.sh
	tofu -chdir=infra/demo fmt -check -recursive
