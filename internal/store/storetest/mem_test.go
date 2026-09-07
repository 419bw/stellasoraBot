package storetest_test

import (
	"testing"

	"xingta/internal/store"
	"xingta/internal/store/storetest"
)

// 内存假件自己也要过契约：功能包的断言建立在它身上，它错了全仓测试都是假绿。
func TestMemDocContract(t *testing.T) {
	storetest.RunDocContract(t, func(t *testing.T) store.Doc {
		t.Helper()
		return storetest.NewMem()
	})
}
