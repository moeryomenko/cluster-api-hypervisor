package systemd

import (
	"context"
	"fmt"

	"github.com/godbus/dbus/v5"
)

const (
	managerPath      = "/org/freedesktop/systemd1"
	managerInterface = "org.freedesktop.systemd1.Manager"
)

type Unit struct {
	Name string
	Path dbus.ObjectPath
	PID  uint32
}

type Property struct {
	Name  string
	Value dbus.Variant
}

type AuxiliaryUnit struct {
	Name       string
	Properties []Property
}

// Client deliberately models only typed Manager API calls. Persistent unit
// files are rendered atomically before Reload/Enable/Start are invoked.
type Client interface {
	Reload(context.Context) error
	EnableUnitFiles(context.Context, []string) error
	StartUnit(context.Context, string) (Unit, error)
	StartTransientUnit(context.Context, string, []Property) (Unit, error)
	StopUnit(context.Context, string) error
	GetUnit(context.Context, string) (Unit, error)
	ListUnits(context.Context) ([]Unit, error)
}

type DBusClient struct {
	conn *dbus.Conn
}

func ConnectUser(address string) (*DBusClient, error) {
	conn, err := dbus.Dial(address)
	if err != nil {
		return nil, fmt.Errorf("dial user D-Bus: %w", err)
	}

	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticate user D-Bus: %w", err)
	}

	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("hello user D-Bus: %w", err)
	}

	return &DBusClient{conn: conn}, nil
}

func (c *DBusClient) Close() error { return c.conn.Close() }

func (c *DBusClient) Reload(ctx context.Context) error {
	call := c.conn.Object(managerInterface, managerPath).CallWithContext(ctx, managerInterface+".Reload", 0)
	if call.Err != nil {
		return fmt.Errorf("reload user systemd manager: %w", call.Err)
	}

	return nil
}

func (c *DBusClient) EnableUnitFiles(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}

	var (
		carriesInstallInfo bool
		changes            []struct {
			Type        string
			Path        string
			Source      string
			Destination string
		}
	)

	call := c.conn.Object(managerInterface, managerPath).
		CallWithContext(ctx, managerInterface+".EnableUnitFiles", 0, names, false, true)
	if call.Err != nil {
		return fmt.Errorf("enable user units: %w", call.Err)
	}

	if err := call.Store(&carriesInstallInfo, &changes); err != nil {
		return fmt.Errorf("decode enable user units: %w", err)
	}

	return nil
}

func (c *DBusClient) StartUnit(ctx context.Context, name string) (Unit, error) {
	var job dbus.ObjectPath

	call := c.conn.Object(managerInterface, managerPath).
		CallWithContext(ctx, managerInterface+".StartUnit", 0, name, "replace")
	if call.Err != nil {
		return Unit{}, fmt.Errorf("start unit %q: %w", name, call.Err)
	}

	if err := call.Store(&job); err != nil {
		return Unit{}, fmt.Errorf("decode start unit %q: %w", name, err)
	}

	unit, err := c.GetUnit(ctx, name)
	if err != nil {
		return Unit{}, err
	}

	return unit, nil
}

func (c *DBusClient) StartTransientUnit(ctx context.Context, name string, properties []Property) (Unit, error) {
	var path dbus.ObjectPath

	call := c.conn.Object(managerInterface, managerPath).
		CallWithContext(ctx, managerInterface+".StartTransientUnit", 0, name, "replace", properties, []AuxiliaryUnit{})
	if call.Err != nil {
		return Unit{}, fmt.Errorf("start transient unit %q: %w", name, call.Err)
	}

	if err := call.Store(&path); err != nil {
		return Unit{}, fmt.Errorf("decode transient unit %q: %w", name, err)
	}

	return Unit{Name: name, Path: path}, nil
}

func (c *DBusClient) StopUnit(ctx context.Context, name string) error {
	var ignored dbus.ObjectPath

	call := c.conn.Object(managerInterface, managerPath).
		CallWithContext(ctx, managerInterface+".StopUnit", 0, name, "replace")
	if call.Err != nil {
		return fmt.Errorf("stop unit %q: %w", name, call.Err)
	}

	if err := call.Store(&ignored); err != nil {
		return fmt.Errorf("decode stop unit %q: %w", name, err)
	}

	return nil
}

func (c *DBusClient) GetUnit(ctx context.Context, name string) (Unit, error) {
	var path dbus.ObjectPath

	call := c.conn.Object(managerInterface, managerPath).CallWithContext(ctx, managerInterface+".GetUnit", 0, name)
	if call.Err != nil {
		return Unit{}, fmt.Errorf("get unit %q: %w", name, call.Err)
	}

	if err := call.Store(&path); err != nil {
		return Unit{}, fmt.Errorf("decode unit %q: %w", name, err)
	}

	unit := Unit{Name: name, Path: path}

	pidProperty, err := c.conn.Object("org.freedesktop.systemd1.Unit", path).
		GetProperty("org.freedesktop.systemd1.Service.MainPID")
	if err == nil {
		unit.PID, _ = pidProperty.Value().(uint32)
	}

	return unit, nil
}

func (c *DBusClient) ListUnits(ctx context.Context) ([]Unit, error) {
	var values []struct {
		Name, Description, LoadState, ActiveState, SubState, Followed string
		Path                                                          dbus.ObjectPath
		JobID                                                         uint32
		JobType                                                       string
		JobPath                                                       dbus.ObjectPath
	}

	call := c.conn.Object(managerInterface, managerPath).CallWithContext(ctx, managerInterface+".ListUnits", 0)
	if call.Err != nil {
		return nil, fmt.Errorf("list units: %w", call.Err)
	}

	if err := call.Store(&values); err != nil {
		return nil, fmt.Errorf("decode units: %w", err)
	}

	units := make([]Unit, 0, len(values))
	for _, value := range values {
		units = append(units, Unit{Name: value.Name, Path: value.Path})
	}

	return units, nil
}
