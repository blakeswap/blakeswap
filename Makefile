.DEFAULT_GOAL := help
APP_DATA_DIR ?= $(HOME)/Library/Application Support/Blakeswap

.PHONY: help reset-local-data test-reset
help:
	@echo 'make reset-local-data  Move app storage to a dated backup; show onboarding on next launch.'
	@echo 'Override APP_DATA_DIR to reset an isolated development installation.'

reset-local-data:
	python3 scripts/reset-local-data.py --data-dir "$(APP_DATA_DIR)"

test-reset:
	python3 -m unittest discover -s scripts -p 'test_reset_local_data.py'
