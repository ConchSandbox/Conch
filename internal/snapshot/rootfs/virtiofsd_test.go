package rootfs

import (
	"testing"
)

func TestNewManager(t *testing.T) {
	m := NewManager()
	err := m.LoadInstances()
	if err != nil {
		t.Errorf("load instances failed:%v\n", err)
	}
	conf := &Config{
		Key: "test",
	}
	ins, err := m.NewInstance(conf)
	if err != nil {
		t.Errorf("new instance failed:%v\n", err)
	}
	err = ins.Start()
	if err != nil {
		t.Errorf("start instance failed:%v\n", err)
	}
	err = ins.Stop()
	if err != nil {
		t.Errorf("stop instance failed:%v\n", err)
	}
	m.RemoveInstance(conf.Key)
}
