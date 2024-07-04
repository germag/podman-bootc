package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/containers/podman-bootc/pkg/bootc"
	"github.com/containers/podman-bootc/pkg/config"
	"github.com/containers/podman-bootc/pkg/define"
	"github.com/containers/podman-bootc/pkg/user"
	"github.com/containers/podman-bootc/pkg/utils"
	"github.com/containers/podman-bootc/pkg/vm"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type osVmConfig struct {
	User            string
	CloudInitDir    string
	KsFile          string
	Background      bool
	NoCredentials   bool
	RemoveVm        bool // Kill the running VM when it exits
	RemoveDiskImage bool // After exit of the VM, remove the disk image
	Quiet           bool
}

var (
	// listCmd represents the hello command
	runCmd = &cobra.Command{
		Use:          "run",
		Short:        "Run a bootc container as a VM",
		Long:         "Run a bootc container as a VM",
		Args:         cobra.MinimumNArgs(1),
		RunE:         doRun,
		SilenceUsage: true,
	}

	vmConfig                = osVmConfig{}
	diskImageConfigInstance = bootc.DiskImageConfig{}
)

func init() {
	RootCmd.AddCommand(runCmd)
	runCmd.Flags().StringVarP(&vmConfig.User, "user", "u", "root", "--user <user name> (default: root)")

	runCmd.Flags().StringVar(&vmConfig.CloudInitDir, "cloudinit", "", "--cloudinit <cloud-init data directory>")

	runCmd.Flags().StringVar(&diskImageConfigInstance.Filesystem, "filesystem", "", "Override the root filesystem (e.g. xfs, btrfs, ext4)")
	runCmd.Flags().BoolVar(&vmConfig.NoCredentials, "no-creds", false, "Do not inject default SSH key via credentials; also implies --background")
	runCmd.Flags().BoolVarP(&vmConfig.Background, "background", "B", false, "Do not spawn SSH, run in background")
	runCmd.Flags().BoolVar(&vmConfig.RemoveVm, "rm", false, "Remove the VM and it's disk when the SSH session exits. Cannot be used with --background")
	runCmd.Flags().BoolVar(&vmConfig.Quiet, "quiet", false, "Suppress output from bootc disk creation and VM boot console")
	runCmd.Flags().StringVar(&diskImageConfigInstance.RootSizeMax, "root-size-max", "", "Maximum size of root filesystem in bytes; optionally accepts M, G, T suffixes")
	runCmd.Flags().StringVar(&diskImageConfigInstance.DiskSize, "disk-size", "", "Allocate a disk image of this size in bytes; optionally accepts M, G, T suffixes")
}

func doRun(flags *cobra.Command, args []string) error {
	//get user info who is running the podman bootc command
	user, err := user.NewUser()
	if err != nil {
		return fmt.Errorf("unable to get user: %w", err)
	}

	//podman machine connection
	machineInfo, err := utils.GetMachineInfo(user)
	if err != nil {
		return err
	}

	if machineInfo == nil {
		println(utils.PodmanMachineErrorMessage)
		return errors.New("rootful podman machine is required, please run 'podman machine init --rootful'")
	}

	if !machineInfo.Rootful {
		println(utils.PodmanMachineErrorMessage)
		return errors.New("rootful podman machine is required, please run 'podman machine set --rootful'")
	}

	if _, err := os.Stat(machineInfo.PodmanSocket); err != nil {
		println(utils.PodmanMachineErrorMessage)
		logrus.Errorf("podman machine socket is missing. Is podman machine running?\n%s", err)
		return err
	}

	ctx, err := bindings.NewConnectionWithIdentity(
		context.Background(),
		fmt.Sprintf("unix://%s", machineInfo.PodmanSocket),
		machineInfo.SSHIdentityPath,
		true)
	if err != nil {
		println(utils.PodmanMachineErrorMessage)
		logrus.Errorf("failed to connect to the podman socket. Is podman machine running?\n%s", err)
		return err
	}

	// create the disk image
	idOrName := args[0]
	bootcDisk := bootc.NewBootcDisk(idOrName, ctx, user)
	err = bootcDisk.Install(vmConfig.Quiet, diskImageConfigInstance)

	if err != nil {
		return fmt.Errorf("unable to install bootc image: %w", err)
	}

	//start the VM
	println("Booting the VM...")
	sshPort, err := utils.GetFreeLocalTcpPort()
	if err != nil {
		return fmt.Errorf("unable to get free port for SSH: %w", err)
	}

	guard, unlock, err := user.Storage().Get(define.FullImageId(bootcDisk.GetImageId()))
	if err != nil {
		return fmt.Errorf("unable to lock the VM cache: %w", err)
	}
	defer func() {
		if err := unlock(); err != nil {
			logrus.Warningf("unable to unlock VM %s: %v", bootcDisk.GetImageId(), err)
		}
	}()

	bootcVM, err := vm.NewVM(vm.NewVMParameters{
		ImageID:    bootcDisk.GetImageId(),
		User:       user,
		LibvirtUri: config.LibvirtUri,
	})

	if err != nil {
		return fmt.Errorf("unable to initialize VM: %w", err)
	}

	// Let's be explicit instead of relying on the defer exec order
	defer func() {
		bootcVM.CloseConnection()
	}()

	cmd := args[1:]
	err = bootcVM.Run(vm.RunVMParameters{
		Cmd:           cmd,
		CloudInitDir:  vmConfig.CloudInitDir,
		NoCredentials: vmConfig.NoCredentials,
		CloudInitData: flags.Flags().Changed("cloudinit"),
		RemoveVm:      vmConfig.RemoveVm,
		Background:    vmConfig.Background,
		SSHPort:       sshPort,
		SSHIdentity:   machineInfo.SSHIdentityPath,
		VMUser:        vmConfig.User,
	})

	if err != nil {
		return fmt.Errorf("runBootcVM: %w", err)
	}

	// write down the config file
	// FIXME: we are writing the config using a shared guard
	if err = bootcVM.WriteConfig(*bootcDisk); err != nil {
		return err
	}

	if !vmConfig.Background {
		if !vmConfig.Quiet {
			var vmConsoleWg sync.WaitGroup
			vmConsoleWg.Add(1)
			go func() {
				err := bootcVM.PrintConsole()
				if err != nil {
					logrus.Errorf("error printing VM console: %v", err)
				}
			}()

			err = bootcVM.WaitForSSHToBeReady()
			if err != nil {
				return fmt.Errorf("WaitSshReady: %w", err)
			}

			vmConsoleWg.Done() //stop printing the VM console when SSH is ready

			// the PrintConsole routine is suddenly stopped without waiting for
			// the print buffer to be flushed, this can lead to the consoel output
			// printing after the ssh prompt begins. Sleeping for a second
			// should prevent this from happening on most systems.
			//
			// The libvirt console stream API blocks while waiting for data, so
			// cleanly stopping the routing via a channel is not possible.
			time.Sleep(1 * time.Second)
		} else {
			err = bootcVM.WaitForSSHToBeReady()
			if err != nil {
				return fmt.Errorf("WaitSshReady: %w", err)
			}
		}

		// ssh into the VM
		ExitCode, err = utils.WithExitCode(bootcVM.RunSSH(guard, cmd))
		if err != nil {
			return fmt.Errorf("ssh: %w", err)
		}

		// Always remove when executing a command
		if vmConfig.RemoveVm || len(cmd) > 0 {
			err = bootcVM.Delete() //delete the VM, but keep the disk image
			if err != nil {
				return fmt.Errorf("unable to remove VM from cache: %w", err)
			}
		}
	}

	return nil
}
