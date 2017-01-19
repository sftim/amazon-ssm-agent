// Copyright 2016 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package executers contains general purpose (shell) command executing objects.
package executers

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	"github.com/aws/amazon-ssm-agent/agent/fileutil"
	"github.com/aws/amazon-ssm-agent/agent/log"
	"github.com/aws/amazon-ssm-agent/agent/task"
)

const (
	// envVar* constants are names of environment variables set for processes executed by ssm agent and should start with AWS_SSM_
	envVarInstanceId = "AWS_SSM_INSTANCE_ID"
	envVarRegionName = "AWS_SSM_REGION_NAME"
)

// T is the interface type for ShellCommandExecuter.
type T interface {
	Execute(log.T, string, string, string, task.CancelFlag, int, string, []string) (io.Reader, io.Reader, int, []error)
	StartExe(log.T, string, string, string, task.CancelFlag, string, []string) (*os.Process, int, []error)
}

// ShellCommandExecuter is specially added for testing purposes
type ShellCommandExecuter struct {
}

type timeoutSignal struct {
	// process kill doesn't send proper signal to the process status
	// Setting the execInterruptedOnWindows to indicate execution was interrupted
	execInterruptedOnWindows bool
}

// Execute executes a list of shell commands in the given working directory.
// If no file path is provided for either stdout or stderr, output will be written to a byte buffer.
// Returns readers for the standard output and standard error streams, process exit code, and a set of errors.
// The errors need not be fatal - the output streams may still have data
// even though some errors are reported. For example, if the command got killed while executing,
// the streams will have whatever data was printed up to the kill point, and the errors will
// indicate that the process got terminated.
func (ShellCommandExecuter) Execute(
	log log.T,
	workingDir string,
	stdoutFilePath string,
	stderrFilePath string,
	cancelFlag task.CancelFlag,
	executionTimeout int,
	commandName string,
	commandArguments []string,
) (stdout io.Reader, stderr io.Reader, exitCode int, errs []error) {

	var stdoutWriter io.Writer
	var stdoutBuf *bytes.Buffer
	if stdoutFilePath != "" {
		// create stdout file
		// fix the permissions appropriately
		// Allow append so that if arrays of run command write to the same file, we keep appending to the file.
		stdoutFileWriter, err := os.OpenFile(stdoutFilePath, appconfig.FileFlagsCreateOrAppend, appconfig.ReadWriteAccess)
		if err != nil {
			return
		}
		stdoutWriter = stdoutFileWriter
		defer stdoutFileWriter.Close()
	} else {
		stdoutBuf = bytes.NewBuffer(nil)
		stdoutWriter = stdoutBuf
	}

	var stderrWriter io.Writer
	var stderrBuf *bytes.Buffer
	if stderrFilePath != "" {
		// create stderr file
		// fix the permissions appropriately
		// Allow append so that if arrays of run command write to the same file, we keep appending to the file.
		stderrFileWriter, err := os.OpenFile(stderrFilePath, appconfig.FileFlagsCreateOrAppend, appconfig.ReadWriteAccess)
		if err != nil {
			return
		}
		stderrWriter = stderrFileWriter
		defer stderrFileWriter.Close() // ExecuteCommand creates a copy of the handle
	} else {
		stderrBuf = bytes.NewBuffer(nil)
		stderrWriter = stderrBuf
	}

	// NOTE: Regarding the defer close of the file writers.
	// Technically, closing the files should happen after ExecuteCommand and before opening the files for reading.
	// In this case, there is no need for that because the child process inherits copies of the file handles and does
	// the actual writing to the files. So, when using files, it does not matter when we close our copies of the file writers.

	var err error
	exitCode, err = ExecuteCommand(log, cancelFlag, workingDir, stdoutWriter, stderrWriter, executionTimeout, commandName, commandArguments)
	if err != nil {
		errs = append(errs, err)
	}

	// create reader from stdout, if it exist, otherwise use the buffer
	if fileutil.Exists(stdoutFilePath) {
		stdout, err = os.Open(stdoutFilePath)
		if err != nil {
			// some unexpected error (file should exist)
			errs = append(errs, err)
		}
	} else {
		stdout = bytes.NewReader(stdoutBuf.Bytes())
	}

	// create reader from stderr, if it exist, otherwise use the buffer
	if fileutil.Exists(stderrFilePath) {
		stderr, err = os.Open(stderrFilePath)
		if err != nil {
			// some unexpected error (file should exist)
			errs = append(errs, err)
		}
	} else {
		stderr = bytes.NewReader(stderrBuf.Bytes())
	}

	return
}

// StartExe starts a list of shell commands in the given working directory.
// Returns process started, an exit code (0 if successfully launch, 1 if error launching process), and a set of errors.
// The errors need not be fatal - the output streams may still have data
// even though some errors are reported. For example, if the command got killed while executing,
// the streams will have whatever data was printed up to the kill point, and the errors will
// indicate that the process got terminated.
func (ShellCommandExecuter) StartExe(
	log log.T,
	workingDir string,
	stdoutFilePath string,
	stderrFilePath string,
	cancelFlag task.CancelFlag,
	commandName string,
	commandArguments []string,
) (process *os.Process, exitCode int, errs []error) {

	var stdoutWriter, stderrWriter *os.File
	if stdoutFilePath != "" {
		// create stdout file
		// fix the permissions appropriately
		// Allow append so that if arrays of run command write to the same file, we keep appending to the file.
		stdoutWriter, err := os.OpenFile(stdoutFilePath, appconfig.FileFlagsCreateOrAppend, appconfig.ReadWriteAccess)
		if err != nil {
			return
		}
		defer stdoutWriter.Close() // Closing our instance of the file handle - the child process has its own copy
	}

	if stderrFilePath != "" {
		// create stderr file
		// fix the permissions appropriately
		// Allow append so that if arrays of run command write to the same file, we keep appending to the file.
		stderrWriter, err := os.OpenFile(stderrFilePath, appconfig.FileFlagsCreateOrAppend, appconfig.ReadWriteAccess)
		if err != nil {
			return
		}
		defer stderrWriter.Close() // Closing our instance of the file handle - the child process has its own copy
	}

	// NOTE: Regarding the defer close of the file writers.
	// The defer will close these file handles before the asynchronous process uses them.
	// In this case, it doesn't cause a problem because the child process inherits copies of the file handles and does
	// the actual writing to the files. So, when using files, it does not matter when we close our copies of the file writers.

	var err error
	process, exitCode, err = StartCommand(log, cancelFlag, workingDir, stdoutWriter, stderrWriter, commandName, commandArguments)
	if err != nil {
		errs = append(errs, err)
	}

	return
}

// CreateScriptFile creates a script containing the given commands.
func CreateScriptFile(scriptPath string, commands []string) (err error) {
	// create script
	file, err := os.Create(scriptPath)
	if err != nil {
		return
	}
	defer file.Close()

	// write commands
	_, err = file.WriteString(strings.Join(commands, "\n") + "\n")
	if err != nil {
		return
	}
	return
}

// ExecuteCommand executes the given commands using the given working directory.
// Standard output and standard error are sent to the given writers.
func ExecuteCommand(log log.T,
	cancelFlag task.CancelFlag,
	workingDir string,
	stdoutWriter io.Writer,
	stderrWriter io.Writer,
	executionTimeout int,
	commandName string,
	commandArguments []string,
) (exitCode int, err error) {

	command := exec.Command(commandName, commandArguments...)
	command.Dir = workingDir
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	exitCode = 0

	// configure OS-specific process settings
	prepareProcess(command)

	// configure environment variables
	prepareEnvironment(command)

	log.Debug()
	log.Debugf("Running in directory %v, command: %v %v.", workingDir, commandName, commandArguments)
	log.Debug()
	if err = command.Start(); err != nil {
		log.Error("error occurred starting the command", err)
		exitCode = 1
		return
	}

	signal := timeoutSignal{}

	go killProcessOnCancel(log, command, cancelFlag, &signal)
	timer := time.NewTimer(time.Duration(executionTimeout) * time.Second)
	go killProcessOnTimeout(log, command, timer, &signal)

	err = command.Wait()
	timedOut := !timer.Stop() // returns false if called previously - indicates timedOut.
	if err != nil {
		exitCode = 1
		log.Debugf("command failed to run %v", err)
		if exiterr, ok := err.(*exec.ExitError); ok {
			// The program has exited with an exit code != 0
			if status, ok := exiterr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()

				if signal.execInterruptedOnWindows {
					log.Debug("command interrupted by cancel or timeout")
					exitCode = -1
				}

				// First try to handle Cancel and Timeout scenarios
				// SIGKILL will result in an exitcode of -1
				if exitCode == -1 {
					if cancelFlag.Canceled() {
						// set appropriate exit code based on cancel or timeout
						exitCode = appconfig.CommandStoppedPreemptivelyExitCode
						log.Infof("The execution of command was cancelled.")
					} else if timedOut {
						// set appropriate exit code based on cancel or timeout
						exitCode = appconfig.CommandStoppedPreemptivelyExitCode
						log.Infof("The execution of command was timedout.")
					}
				} else {
					log.Infof("The execution of command returned Exit Status: %d", exitCode)
				}
			}
		}
	} else {
		// check if cancellation or timeout failed to kill the process
		// This will not occur as we do a SIGKILL, which is not recoverable.
		if cancelFlag.Canceled() {
			// This is when the cancellation failed and the command completed successfully
			log.Errorf("the cancellation failed to stop the process.")
			// do not return as the command could have been cancelled and also timedout
		}
		if timedOut {
			// This is when the timeout failed and the command completed successfully
			log.Errorf("the timeout failed to stop the process.")
		}
	}

	log.Debug("Done waiting!")
	return
}

// StartCommand starts the given commands using the given working directory.
// Standard output and standard error are sent to the given writers.
func StartCommand(log log.T,
	cancelFlag task.CancelFlag,
	workingDir string,
	stdoutWriter io.Writer,
	stderrWriter io.Writer,
	commandName string,
	commandArguments []string,
) (process *os.Process, exitCode int, err error) {

	command := exec.Command(commandName, commandArguments...)
	command.Dir = workingDir
	command.Stdout = stdoutWriter
	command.Stderr = stderrWriter
	exitCode = 0

	// configure OS-specific process settings
	prepareProcess(command)

	// configure environment variables
	prepareEnvironment(command)

	log.Debug()
	log.Debugf("Running in directory %v, command: %v %v.", workingDir, commandName, commandArguments)
	log.Debug()
	if err = command.Start(); err != nil {
		log.Error("error occurred starting the command: ", err)
		exitCode = 1
		return
	}

	process = command.Process
	signal := timeoutSignal{}
	go killProcessOnCancel(log, command, cancelFlag, &signal)

	return
}

// killProcessOnCancel waits for a cancel request.
// If a cancel request is received, this method kills the underlying
// process of the command. This will unblock the command.Wait() call.
// If the task completed successfully this method returns with no action.
func killProcessOnCancel(log log.T, command *exec.Cmd, cancelFlag task.CancelFlag, signal *timeoutSignal) {
	cancelFlag.Wait()
	if cancelFlag.Canceled() {
		log.Debug("Process cancelled. Attempting to stop process.")

		// task has been asked to cancel, kill process
		if err := killProcess(command.Process, signal); err != nil {
			log.Error(err)
			return
		}

		log.Debug("Process stopped successfully.")
	}
}

// killProcessOnTimeout waits for a timeout.
// When the timeout is reached, this method kills the underlying
// process of the command. This will unblock the command.Wait() call.
// If the task completed successfully this method returns with no action.
func killProcessOnTimeout(log log.T, command *exec.Cmd, timer *time.Timer, signal *timeoutSignal) {
	<-timer.C
	log.Debug("Process exceeded timeout. Attempting to stop process.")

	// task has been exceeded the allowed execution timeout, kill process
	if err := killProcess(command.Process, signal); err != nil {
		log.Error(err)
		return
	}

	log.Debug("Process stopped successfully")
}

// prepareEnvironment adds ssm agent standard environment variables to the command
func prepareEnvironment(command *exec.Cmd) {
	env := os.Environ()
	if instance, err := instance.InstanceID(); err == nil {
		env = append(env, fmtEnvVariable(envVarInstanceId, instance))
	}
	if region, err := instance.Region(); err == nil {
		env = append(env, fmtEnvVariable(envVarRegionName, region))
	}
	command.Env = env

	// Running powershell on linux erquired the HOME env variable to be set and to remove the TERM env variable
	validateEnvironmentVariables(command)
}

// fmtEnvVariable creates the string to append to the current set of environment variables.
func fmtEnvVariable(name string, val string) string {
	return fmt.Sprintf("%s=%s", name, val)
}
