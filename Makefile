# The on-demand AWS demo environment (docs/design.md, section 13). Each target needs a
# signed-in AWS profile: aws sso login --profile <PROJECT_PROFILE in infra/bootstrap/aws/config.env>
.PHONY: demo-up demo-smoke demo-spike demo-stepup demo-throughput demo-crash demo-fault demo-nats-sync demo-status demo-down demo-check demo-test

demo-up:    ## create the environment, ship the code, smoke-test it (about $0.23/hour until demo-down)
	./infra/demo/up.sh
demo-smoke: ## re-run the end-to-end smoke test
	./infra/demo/smoke.sh
demo-spike: ## a webhook spike from the load generator, then checks nothing was lost (RATE, HOLD)
	./infra/demo/spike.sh
demo-stepup: ## edge step-up: rate steps until p99 >= 50 ms; consumers paused, stream reset after (STEPS, STEP)
	./infra/demo/stepup.sh
demo-throughput: ## attribution's drain rate on a queued backlog, with CPU and a profile (RATE, QUEUE)
	./infra/demo/throughput.sh
demo-crash: ## the crash test on the server, isolated from the demo's data (KILLS=0 for the clean run)
	./infra/demo/crash.sh
demo-fault: ## silent-failure drill: time to the alert, then replay and check nothing is lost
	./infra/demo/fault.sh
demo-nats-sync: ## JetStream's disk sync: SYNC=always or SYNC=default (every 2 minutes); restarts NATS
	./infra/demo/remote-nats-sync.sh
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
