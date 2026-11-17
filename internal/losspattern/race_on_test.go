//go:build race

package losspattern

// raceBuild says the race detector is on. The simulations are arithmetic on
// one goroutine: it has nothing to find in them and makes them ten times
// slower, minutes on a CI runner. Under it they run a token number of windows
// and do not judge the rates; CI runs them at full size in a step of their own.
const raceBuild = true
