module github.com/camilorivera/cert-manager-acm-sync

go 1.23

require (
	github.com/aws/aws-sdk-go-v2 v1.32.3
	github.com/aws/aws-sdk-go-v2/config v1.28.3
	github.com/aws/aws-sdk-go-v2/service/acm v1.30.3
	github.com/aws/smithy-go v1.22.0
	github.com/prometheus/client_golang v1.20.4
	github.com/stretchr/testify v1.9.0
	go.uber.org/zap v1.27.0
	k8s.io/api v0.31.3
	k8s.io/apimachinery v0.31.3
	k8s.io/client-go v0.31.3
	sigs.k8s.io/controller-runtime v0.19.3
)
