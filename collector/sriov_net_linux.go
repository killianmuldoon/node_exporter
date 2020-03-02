//+build !nosriovnet
package collector

import (
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/go-kit/kit/log"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"

	"os"
)

const (
	sriovStatSubsystem = "sriovnet"
	sysBusPci = "/sys/bus/pci/devices"
	totalVfFile      = "sriov_totalvfs"
	pfNameFile = "/net"
	netClassFile = "/class"
	driverFile = "/driver"
	netClass = 0x020000
)


func init() {
	registerCollector("sriovnet", defaultDisabled, NewSriovNetCollector)
}


//vfList contains a list of addresses for VFs with the name of the physical interface as value
type vfWithRoot map[string]string
type sriovStats map[string]float64


//sriovNetCollector implements the collector interface to be picked up by node exporter.
type sriovNetCollector struct {
	logger       log.Logger
}
//sriovStatReader is an interface which takes in the physical function name and vf id and returns the stats for the VF
type sriovStatReader interface {
	ReadStats(vfID string, pfName string) sriovStats
}

//NewSriovNetCollector returns the collector required for registration with node exporter
func NewSriovNetCollector(logger log.Logger) (Collector, error){
	s :=  &sriovNetCollector{
		logger: logger,
	}
	return s , nil
}

//statReaderForPF returns the correct stat reader for the given PF
//currently only i40e is implemented, but other drivers can be implemented and picked up here.
func statReaderForPF (pf string) sriovStatReader {
	pfDriverPath := filepath.Join(sysBusPci, pf, driverFile)
	driverInfo, _ := os.Readlink(pfDriverPath)
	pfDriver := filepath.Base(driverInfo)
	switch pfDriver {
	case "i40e":
		return i40eReader{}
	default:
		return nil
	}
}
// Update looks for all SRIOV Network PFs on the system, looks for the VFs for each, and reports per VF stats.
func (c *sriovNetCollector) Update(ch chan<- prometheus.Metric) error {
	pfList, err := c.getSriovPFs()
	if err != nil {
		return err
	}
	for _, pf := range pfList {
		reader := statReaderForPF(pf)
		if reader == nil {
			continue
		}
		vfs, err  := vfList(pf)
		if err != nil{
			continue
		}
		pfName := getPFName(pf)
		for id, address := range vfs {
			stats := reader.ReadStats(pfName,id)
			for name, v := range stats {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, sriovStatSubsystem, name),
					fmt.Sprintf("Statistic %s.", name),
					[]string{"pfName","vf","vfAddress"},nil,
				)
				ch <- prometheus.MustNewConstMetric(
					desc,
					prometheus.CounterValue,
					v,
					pfName,
					id,
					address,
				)
			}
		}
	}
	return nil
}

//getSriovPFs returns the SRIOV capable Physical Network functions for the host
func (c sriovNetCollector )getSriovPFs() ([]string , error) {
	sriovPFs := make([]string, 0)
	devs := getPCIDevs()
	if len(devs) == 0 {
		return sriovPFs, errors.New("pci devices could not be found")
	}
	for _, device := range devs {
		if c.isSriovNetPF(device.Name()) {
			sriovPFs = append(sriovPFs, device.Name())
		}
	}
	if len(sriovPFs) == 0 {
		return sriovPFs, errors.New("no sriov net devices found on host")
	}
	return sriovPFs , nil
}

// IsSriovPF checks if is device SRIOV capable net device. It checks if the sriov_totalvfs file exists for the given PCI address
func (c sriovNetCollector) isSriovNetPF(pciAddr string) bool {
	totalVfFilePath := filepath.Join(sysBusPci, pciAddr, totalVfFile)
	devClassFilePath := filepath.Join(sysBusPci,pciAddr,netClassFile)
	if !c.isNetDevice(devClassFilePath){
		return false
	}
	if _, err := os.Stat(totalVfFilePath); err != nil {
		return false
	}
	return true
}

// isNetDevice checks if the device is a net device by checking its device class
func (c sriovNetCollector) isNetDevice (filepath string) bool {

	file, err := ioutil.ReadFile(filepath)
	if err != nil {
		return false
	}
	classHex := strings.TrimSpace(string(file))
	deviceClass, err := strconv.ParseInt(classHex , 0, 64 )
	if err != nil {
		return false
	}
	return deviceClass == netClass
}

// get PCIDevs returns all of the PCI device files available on the host
func getPCIDevs () []os.FileInfo {
	links, err := ioutil.ReadDir(sysBusPci)
	if err != nil {
		return make([]os.FileInfo,0)
	}
	return links
}

//getVFsFromPF returns the Virtual Functions associated with a specific SRIOV Physical Function
func vfList(pfAddress string) (vfWithRoot, error) {
	vfList := make(vfWithRoot, 0)
	pfDir := filepath.Join(sysBusPci, pfAddress)
	_, err := os.Lstat(pfDir)
	if err != nil {
		err = fmt.Errorf("could not get PF directory information for device: %s, Err: %v", pfAddress, err)
		return vfList, err
	}
	vfDirs, err := filepath.Glob(filepath.Join(pfDir, "virtfn*"))
	if err != nil {
		err = fmt.Errorf("error reading VF directories %v", err)
		return vfList, err
	}
	//Read all VF directory and get add VF PCI addr to the vfList
	for _, dir := range vfDirs {
		dirInfo, err := os.Lstat(dir)
		if err == nil && (dirInfo.Mode()&os.ModeSymlink != 0) {
			linkName, err := filepath.EvalSymlinks(dir)
			if err == nil {
				vfLink := filepath.Base(linkName)
				vfID := dirInfo.Name()[6:]
				vfList[vfID] = vfLink
				}
			}
		}

	return vfList, nil
}

//getPFName resolves the system's name for a physical interface from the PCI address linked to it.
func getPFName (device string) string {
	pfdir, err  := ioutil.ReadDir(filepath.Join(sysBusPci,device,pfNameFile))
	if err != nil {
		return ""
	}
	return pfdir[0].Name()
}

//i40eReader is able to read stats from Physical functions running the i40e driver.
type i40eReader struct {
}

//ReadStats takes in the name of a PF and the VF Id and returns a stats object.
func (r i40eReader) ReadStats(pfName string, vfID string ) sriovStats {
	stats := make(sriovStats, 0)
	statroot   := fmt.Sprintf("/sys/class/net/%s/device/sriov/%s/stats/", pfName, vfID)
	files , err := ioutil.ReadDir(statroot)
	if err != nil {
		return stats
	}
	for _, f := range files {
		path:= filepath.Join(statroot,f.Name())
		statRaw, err  := ioutil.ReadFile(path)
		if err != nil{
			continue
		}
		statString := strings.TrimSpace(string(statRaw))
		value, err := strconv.ParseFloat(statString,64)
		if err != nil {
			continue
		}
		stats[f.Name()] = value
	}
	return stats
}