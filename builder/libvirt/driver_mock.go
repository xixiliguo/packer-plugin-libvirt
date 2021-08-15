package libvirt

import "sync"

type DriverMock struct {
	sync.Mutex

	CopyCalled bool
	CopyErr    error

	StopCalled bool
	StopErr    error

	LibvirtCalls [][]string
	LibvirtErrs  []error

	WaitForShutdownCalled bool
	WaitForShutdownState  bool

	QemuImgCalled bool
	QemuImgCalls  []string
	QemuImgErrs   []error

	VerifyCalled bool
	VerifyErr    error

	VersionCalled bool
	VersionResult string
	VersionErr    error
}

func (d *DriverMock) Copy(source, dst string) error {
	d.CopyCalled = true
	return d.CopyErr
}

func (d *DriverMock) Stop() error {
	d.StopCalled = true
	return d.StopErr
}

func (d *DriverMock) Start(args ...string) error {
	d.LibvirtCalls = append(d.LibvirtCalls, args)

	if len(d.LibvirtErrs) >= len(d.LibvirtCalls) {
		return d.LibvirtErrs[len(d.LibvirtCalls)-1]
	}
	return nil
}

func (d *DriverMock) WaitForShutdown(cancelCh <-chan struct{}) bool {
	d.WaitForShutdownCalled = true
	return d.WaitForShutdownState
}

func (d *DriverMock) QemuImg(args ...string) error {
	d.QemuImgCalled = true
	d.QemuImgCalls = append(d.QemuImgCalls, args...)

	if len(d.QemuImgErrs) >= len(d.QemuImgCalls) {
		return d.QemuImgErrs[len(d.QemuImgCalls)-1]
	}
	return nil
}

func (d *DriverMock) Verify() error {
	d.VerifyCalled = true
	return d.VerifyErr
}

func (d *DriverMock) Version() (string, error) {
	d.VersionCalled = true
	return d.VersionResult, d.VersionErr
}
