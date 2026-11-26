module github.com/qooqapp/qooq-cli

go 1.26.0

toolchain go1.26.11

require github.com/spf13/cobra v1.10.2

require gopkg.in/yaml.v3 v3.0.1 // indirect

require (
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/qooqapp/qooq v0.0.0
	github.com/spf13/pflag v1.0.10 // indirect
)

replace github.com/qooqapp/qooq => ../qooq
