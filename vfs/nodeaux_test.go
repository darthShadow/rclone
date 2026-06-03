package vfs

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAux(t *testing.T) {
	var a aux
	owner1, owner2 := new(int), new(int)

	// Nothing attached yet
	assert.Nil(t, a.Aux(owner1))
	assert.Nil(t, a.Sys())

	// Values attached by different owners are independent even if
	// they have different types
	a.SetAux(owner1, "potato")
	a.SetAux(owner2, 2)
	assert.Equal(t, "potato", a.Aux(owner1))
	assert.Equal(t, 2, a.Aux(owner2))

	// Replace a value
	a.SetAux(owner1, "sausage")
	assert.Equal(t, "sausage", a.Aux(owner1))

	// Remove a value
	a.SetAux(owner1, nil)
	assert.Nil(t, a.Aux(owner1))
	assert.Equal(t, 2, a.Aux(owner2))

	// Sys is independent of the other owners
	assert.Nil(t, a.Sys())
	a.SetSys(42)
	assert.Equal(t, 42, a.Sys())
	assert.Equal(t, 2, a.Aux(owner2))

	// Changing the type of the value stored must not panic
	a.SetSys("42")
	assert.Equal(t, "42", a.Sys())

	// Remove the remaining values
	a.SetSys(nil)
	a.SetAux(owner2, nil)
	assert.Nil(t, a.entries.Load())
}

func TestAuxLoadOrStore(t *testing.T) {
	var a aux
	owner := new(int)
	otherOwner := new(int)
	a.SetAux(otherOwner, "other")
	values := []any{"first", "second"}
	actual := make([]any, len(values))
	loaded := make([]bool, len(values))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, value := range values {
		wg.Go(func() {
			<-start
			actual[i], loaded[i] = a.LoadOrStoreAux(owner, value)
		})
	}
	close(start)
	wg.Wait()

	assert.Equal(t, actual[0], actual[1])
	assert.Equal(t, 1, countTrue(loaded), "exactly one caller should load the stored value")
	assert.Equal(t, actual[0], a.Aux(owner))
	assert.Equal(t, "other", a.Aux(otherOwner))

	nilOwner := new(int)
	actualNil, loadedNil := a.LoadOrStoreAux(nilOwner, nil)
	assert.Nil(t, actualNil)
	assert.False(t, loadedNil)
	assert.Nil(t, a.Aux(nilOwner))
}

func countTrue(values []bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func TestAuxConcurrent(t *testing.T) {
	const (
		owners     = 4
		iterations = 100
	)
	var (
		a  aux
		wg sync.WaitGroup
	)
	for i := range owners {
		wg.Go(func() {
			owner := &i
			for j := range iterations {
				value := fmt.Sprintf("%d-%d", i, j)
				a.SetAux(owner, value)
				assert.Equal(t, value, a.Aux(owner))
			}
		})
	}
	wg.Wait()
}
