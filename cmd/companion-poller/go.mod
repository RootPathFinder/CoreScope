module github.com/corescope/companion-poller

go 1.22

require (
	github.com/meshcore-analyzer/companion v0.0.0
	github.com/meshcore-analyzer/repeatervault v0.0.0
)

require golang.org/x/sys v0.30.0 // indirect

replace github.com/meshcore-analyzer/companion => ../../internal/companion

replace github.com/meshcore-analyzer/repeatervault => ../../internal/repeatervault
