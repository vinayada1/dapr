module app

go 1.14

require (
	github.com/dapr/dapr v0.7.1
)

replace k8s.io/client => github.com/kubernetes-client/go v0.0.0-20190928040339-c757968c4c36
