.DEFAULT_GOAL := help
APP_DATA_DIR ?= $(HOME)/Library/Application Support/Blakeswap

.PHONY: help reset-local-data test-reset
help:
	@echo 'make regtest-nodes     Start and register BTC + Blake2b regtest nodes.'
	@echo 'make regtest-btc / regtest-blake   Start and register one chain.'
	@echo 'make regtest-stop      Stop this checkout’s regtest nodes.'
	@echo 'make reset-local-data  Move app storage to a dated backup; show onboarding on next launch.'
	@echo 'Override APP_DATA_DIR to reset an isolated development installation.'

reset-local-data:
	python3 scripts/reset-local-data.py --data-dir "$(APP_DATA_DIR)"

test-reset:
	python3 -m unittest discover -s scripts -p 'test_reset_local_data.py'

.PHONY: regtest-nodes regtest-btc regtest-blake regtest-stop test-local-nodes test-packaging
regtest-nodes:
	python3 scripts/bootstrap.py
	python3 scripts/local.py nodes --register

regtest-btc:
	python3 scripts/bootstrap.py btc
	python3 scripts/local.py nodes btc --register

regtest-blake:
	python3 scripts/bootstrap.py blake
	python3 scripts/local.py nodes blake --register

regtest-stop:
	python3 scripts/local.py stop-nodes

test-local-nodes:
	python3 -m unittest discover -s scripts -p 'test_local_nodes.py'

test-packaging:
	python3 -m unittest discover -s scripts -p 'test_packaging.py'
