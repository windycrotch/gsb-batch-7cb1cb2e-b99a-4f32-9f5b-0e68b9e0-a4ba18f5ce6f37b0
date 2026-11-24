// Package acceptance contains real-UDP end-to-end acceptance tests for the
// udpsocket session lifecycle.
//
// The tests themselves are gated behind the "acceptance" build tag because
// they are intentionally timing- and concurrency-heavy. Run them with:
//
//	go test -tags acceptance -race ./acceptance/
package acceptance
