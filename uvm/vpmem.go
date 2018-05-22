package uvm

import (
	"fmt"
	"strconv"

	"github.com/Microsoft/hcsshim/internal/schema2"
	"github.com/Microsoft/hcsshim/uvm/schema"
	"github.com/sirupsen/logrus"
)

// TODO: Casing on the host path, same as VSMB

// allocateVPMEM finds the next available VPMem slot. The lock MUST be held
// when calling this function.
func (uvm *UtilityVM) allocateVPMEM(hostPath string) (uint32, error) {
	for index, vi := range uvm.vpmemDevices.vpmemInfo {
		if vi.hostPath == "" {
			vi.hostPath = hostPath
			logrus.Debugf("uvm::allocateVPMEM %d %q", index, hostPath)
			return uint32(index), nil
		}
	}
	return 0, fmt.Errorf("no free VPMEM locations")
}

func (uvm *UtilityVM) deallocateVPMEM(deviceNumber uint32) error {
	uvm.vpmemDevices.Lock()
	defer uvm.vpmemDevices.Unlock()
	uvm.vpmemDevices.vpmemInfo[deviceNumber] = vpmemInfo{}
	return nil
}

// Lock must be held when calling this function
func (uvm *UtilityVM) findVPMEMDevice(findThisHostPath string) (uint32, string, error) {
	for deviceNumber, vi := range uvm.vpmemDevices.vpmemInfo {
		if vi.hostPath == findThisHostPath {
			logrus.Debugf("uvm::findVPMEMDeviceNumber %d %s", deviceNumber, findThisHostPath)
			return uint32(deviceNumber), vi.uvmPath, nil
		}
	}
	return 0, "", fmt.Errorf("%s is not attached to VPMEM", findThisHostPath)
}

// AddVPMEM adds a VPMEM disk to a utility VM at the next available location.
//
// This is only supported for v2 schema linux utility VMs
//
// Returns the location(0..255) where the device is attached, and if exposed,
// the container path which will be /tmp/vpmem<location>/ if no container path
// is supplied, or the user supplied one if it is.
func (uvm *UtilityVM) AddVPMEM(hostPath string, uvmPath string, expose bool) (uint32, string, error) {
	if uvm.operatingSystem != "linux" {
		return 0, "", errNotSupported
	}

	logrus.Debugf("uvm::AddVPMEM id:%s hostPath:%s containerPath:%s expose:%t", uvm.id, hostPath, uvmPath, expose)

	uvm.vpmemDevices.Lock()
	defer uvm.vpmemDevices.Unlock()

	var deviceNumber uint32
	var err error
	currentUVMPath := ""

	deviceNumber, currentUVMPath, err = uvm.findVPMEMDevice(hostPath)
	if err != nil {
		// It doesn't exist, so we're going to allocate and hot-add it
		deviceNumber, err = uvm.allocateVPMEM(hostPath)
		if err != nil {
			return 0, "", err
		}
		controller := schema2.VirtualMachinesResourcesStorageVpmemControllerV2{}
		controller.Devices = make(map[string]schema2.VirtualMachinesResourcesStorageVpmemDeviceV2)
		controller.Devices[strconv.Itoa(int(deviceNumber))] = schema2.VirtualMachinesResourcesStorageVpmemDeviceV2{
			HostPath:    hostPath,
			ReadOnly:    true,
			ImageFormat: "VHD1",
		}

		modification := &schema2.ModifySettingsRequestV2{
			ResourceType: schema2.ResourceTypeVPMemDevice,
			RequestType:  schema2.RequestTypeAdd,
			Settings:     controller,
			ResourceUri:  fmt.Sprintf("virtualmachine/devices/virtualpmemdevices/%d", deviceNumber),
		}

		if expose || uvmPath != "" {
			if uvmPath == "" {
				uvmPath = fmt.Sprintf("/tmp/vpmem%d", deviceNumber)
			}
			modification.HostedSettings = schema.LCOWMappedVPMemDevice{
				DeviceNumber: deviceNumber,
				MountPath:    uvmPath,
			}
		}
		currentUVMPath = uvmPath

		if err := uvm.Modify(modification); err != nil {
			uvm.vpmemDevices.vpmemInfo[deviceNumber] = vpmemInfo{}
			return 0, "", fmt.Errorf("uvm::AddVPMEM: failed to modify utility VM configuration: %s", err)
		}

		uvm.vpmemDevices.vpmemInfo[deviceNumber] = vpmemInfo{
			hostPath: hostPath,
			refCount: 1,
			uvmPath:  uvmPath}

		return deviceNumber, uvmPath, nil
	} else {
		pmemi := vpmemInfo{
			hostPath: hostPath,
			refCount: uvm.vpmemDevices.vpmemInfo[deviceNumber].refCount + 1,
			uvmPath:  currentUVMPath}
		uvm.vpmemDevices.vpmemInfo[deviceNumber] = pmemi
	}
	logrus.Debugf("hcsshim::AddVPMEM id:%s Success %+v", uvm.id, uvm.vpmemDevices.vpmemInfo[deviceNumber])
	return deviceNumber, currentUVMPath, nil

}

// RemoveVPMEM removes a VPMEM disk from a utility VM. As an external API, it
// is "safe". Internal use can call removeVPMEM.
func (uvm *UtilityVM) RemoveVPMEM(hostPath string) error {
	if uvm.operatingSystem != "linux" {
		return errNotSupported
	}

	uvm.vpmemDevices.Lock()
	defer uvm.vpmemDevices.Unlock()

	// Make sure is actually attached
	deviceNumber, uvmPath, err := uvm.findVPMEMDevice(hostPath)
	if err != nil {
		return fmt.Errorf("cannot remove VPMEM %s as it is not attached to utility VM %s: %s", hostPath, uvm.id, err)
	}

	if err := uvm.removeVPMEM(hostPath, uvmPath, deviceNumber); err != nil {
		return fmt.Errorf("failed to remove VPMEM %s from utility VM %s: %s", hostPath, uvm.id, err)
	}
	return nil
}

// removeVPMEM is the internally callable "unsafe" version of RemoveVPMEM. The mutex
// MUST be held when calling this function.
func (uvm *UtilityVM) removeVPMEM(hostPath string, uvmPath string, deviceNumber uint32) error {
	logrus.Debugf("uvm::RemoveVPMEM id:%s hostPath:%s", uvm.id, hostPath)

	// TODO: Add the refcounting the same as vsm
	// TODO: Add the remoteType

	modification := &schema2.ModifySettingsRequestV2{
		ResourceType: schema2.ResourceTypeVPMemDevice,
		RequestType:  schema2.RequestTypeRemove,
		ResourceUri:  fmt.Sprintf("virtualmachine/devices/virtualpmemdevices/%d", deviceNumber),
	}

	modification.HostedSettings = schema.LCOWMappedVPMemDevice{
		DeviceNumber: deviceNumber,
		MountPath:    uvmPath,
	}

	if err := uvm.Modify(modification); err != nil {
		return err
	}
	uvm.vpmemDevices.vpmemInfo[deviceNumber] = vpmemInfo{}
	logrus.Debugf("uvm::RemoveVPMEM: Success %s removed from %s %d", hostPath, uvm.id, deviceNumber)
	return nil
}