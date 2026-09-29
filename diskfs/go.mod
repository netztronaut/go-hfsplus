module github.com/netztronaut/go-hfsplus-reader/diskfs

go 1.25.0

require github.com/google/uuid v1.6.0 // indirect

require (
	github.com/diskfs/go-diskfs v1.9.4
	github.com/netztronaut/go-hfsplus-reader v0.0.0
)

// The adapter is developed beside the readers; a release pins a tagged version instead.
replace github.com/netztronaut/go-hfsplus-reader => ../
