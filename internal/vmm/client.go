package vmm

import (
	"fmt"

	"github.com/openeuler/Conch/internal/vmm/clh"
	"github.com/openeuler/Conch/internal/vmm/driver"
	"github.com/openeuler/Conch/internal/vmm/stratovirt"
	"github.com/openeuler/Conch/pkg/ulog"
)

const (
	CLHVmmType        = 0
	StratovirtVmmType = 1

	CloudHypervisorName = "cloud-hypervisor"
	StratovirtName      = "stratovirt"
)

var vmmTypeMap = map[string]int{
	CloudHypervisorName: CLHVmmType,
	StratovirtName:      StratovirtVmmType,
}

func GetVmmType(vmmName string) (int, bool) {
	logger := ulog.GetLogger()
	logger.Debug("Getting VMM type", ulog.F("vmm_name", vmmName))
	vmmType, exists := vmmTypeMap[vmmName]
	return vmmType, exists
}

type ResourceArgs = driver.ResourceArgs
type vmmAdapter = driver.Adapter

func newVmmAdapter(vmmName, vmmSocketPath, vmmBinary string) (vmmAdapter, error) {
	vmmType, exists := GetVmmType(vmmName)
	if !exists {
		return nil, fmt.Errorf("invalid vmm type: %s", vmmName)
	}
	if vmmBinary == "" {
		return nil, fmt.Errorf("vmm %q binary is not configured", vmmName)
	}
	return newVmmAdapterByType(vmmType, vmmSocketPath, vmmBinary)
}

func newVmmAdapterByType(vmmType int, vmmSocketPath, vmmBinary string) (vmmAdapter, error) {
	switch vmmType {
	case CLHVmmType:
		logger := ulog.GetLogger()
		logger.Info("Creating CLH client", ulog.F("socket", vmmSocketPath))
		return clh.NewCLHClient(vmmType, vmmSocketPath, vmmBinary), nil
	case StratovirtVmmType:
		ulog.GetLogger().Info("Creating Stratovirt client", ulog.F("socket", vmmSocketPath))
		return stratovirt.NewStratovirtClient(vmmType, vmmSocketPath, vmmBinary), nil
	default:
		ulog.GetLogger().Error("Unknown VMM type",
			ulog.F("type", vmmType),
		)
		return nil, fmt.Errorf("unknown VMM type: %d", vmmType)
	}
}
