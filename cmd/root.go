// Copyright © 2017 Microsoft <wastore@microsoft.com>
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package cmd

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-azcopy/ste"
	"github.com/Azure/azure-storage-blob-go/azblob"
	"github.com/spf13/cobra"
)

var azcopyAppPathFolder string
var azcopyLogPathFolder string
var azcopyJobPlanFolder string
var azcopyMaxFileAndSocketHandles int
var outputFormatRaw string
var cancelFromStdin bool
var azcopyOutputFormat common.OutputFormat
var cmdLineCapMegaBitsPerSecond uint32

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Version: common.AzcopyVersion, // will enable the user to see the version info in the standard posix way: --version
	Use:     "azcopy",
	Short:   rootCmdShortDescription,
	Long:    rootCmdLongDescription,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {

		err := azcopyOutputFormat.Parse(outputFormatRaw)
		glcm.SetOutputFormat(azcopyOutputFormat)
		if err != nil {
			return err
		}

		// currently, we only automatically do auto-tuning when benchmarking
		preferToAutoTuneGRs := cmd == benchCmd // TODO: do we have a better way to do this than making benchCmd global?
		providePerformanceAdvice := cmd == benchCmd

		// startup of the STE happens here, so that the startup can access the values of command line parameters that are defined for "root" command
		concurrencySettings := ste.NewConcurrencySettings(azcopyMaxFileAndSocketHandles, preferToAutoTuneGRs)
		err = ste.MainSTE(concurrencySettings, int64(cmdLineCapMegaBitsPerSecond), azcopyJobPlanFolder, azcopyLogPathFolder, providePerformanceAdvice)
		if err != nil {
			return err
		}

		// spawn a routine to fetch and compare the local application's version against the latest version available
		// if there's a newer version that can be used, then write the suggestion to stderr
		// however if this takes too long the message won't get printed
		// Note: this function is neccessary for non-help, non-login commands, since they don't reach the corresponding
		// beginDetectNewVersion call in Execute (below)
		beginDetectNewVersion()

		return nil
	},
}

// hold a pointer to the global lifecycle controller so that commands could output messages and exit properly
var glcm = common.GetLifecycleMgr()

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute(azsAppPathFolder, logPathFolder string, jobPlanFolder string, maxFileAndSocketHandles int) {
	azcopyAppPathFolder = azsAppPathFolder
	azcopyLogPathFolder = logPathFolder
	azcopyJobPlanFolder = jobPlanFolder
	azcopyMaxFileAndSocketHandles = maxFileAndSocketHandles

	if err := rootCmd.Execute(); err != nil {
		glcm.Error(err.Error())
	} else {
		// our commands all control their own life explicitly with the lifecycle manager
		// only commands that don't explicitly exit actually reach this point (e.g. help commands and login commands)
		select {
		case <-beginDetectNewVersion():
			// noop
		case <-time.After(time.Second * 8):
			// don't wait too long
		}
		glcm.Exit(nil, common.EExitCode.Success())
	}
}

func init() {
	// replace the word "global" to avoid confusion (e.g. it doesn't affect all instances of AzCopy)
	rootCmd.SetUsageTemplate(strings.Replace((&cobra.Command{}).UsageTemplate(), "Global Flags", "Flags Applying to All Commands", -1))

	rootCmd.PersistentFlags().Uint32Var(&cmdLineCapMegaBitsPerSecond, "cap-mbps", 0, "Caps the transfer rate, in megabits per second. Moment-by-moment throughput might vary slightly from the cap. If this option is set to zero, or it is omitted, the throughput isn't capped.")
	rootCmd.PersistentFlags().StringVar(&outputFormatRaw, "output-type", "text", "Format of the command's output. The choices include: text, json. The default value is 'text'.")

	// Note: this is due to Windows not supporting signals properly
	rootCmd.PersistentFlags().BoolVar(&cancelFromStdin, "cancel-from-stdin", false, "Used by partner teams to send in `cancel` through stdin to stop a job.")

	// reserved for partner teams
	rootCmd.PersistentFlags().MarkHidden("cancel-from-stdin")
}

// always spins up a new goroutine, because sometimes the aka.ms URL can't be reached (e.g. a constrained environment where
// aka.ms is not resolvable to a reachable IP address). In such cases, this routine will run for ever, and the caller should
// just give up on it.
// We spin up the GR here, not in the caller, so that the need to use a separate GC can never be forgotten
// (if do it synchronously, and can't resolve URL, this blocks caller for ever)
func beginDetectNewVersion() chan struct{} {
	completionChannel := make(chan struct{})
	go func() {
		const versionMetadataUrl = "https://aka.ms/azcopyv10-version-metadata"

		// step 0: check the Stderr before checking version
		_, err := os.Stderr.Stat()
		if err != nil {
			return
		}

		// step 1: initialize pipeline
		p, err := createBlobPipeline(context.TODO(), common.CredentialInfo{CredentialType: common.ECredentialType.Anonymous()})
		if err != nil {
			return
		}

		// step 2: parse source url
		u, err := url.Parse(versionMetadataUrl)
		if err != nil {
			return
		}

		// step 3: start download
		blobURL := azblob.NewBlobURL(*u, p)
		blobStream, err := blobURL.Download(context.TODO(), 0, azblob.CountToEnd, azblob.BlobAccessConditions{}, false)
		if err != nil {
			return
		}

		blobBody := blobStream.Body(azblob.RetryReaderOptions{MaxRetryRequests: ste.MaxRetryPerDownloadBody})
		defer blobBody.Close()

		// step 4: read newest version str
		buf := new(bytes.Buffer)
		n, err := buf.ReadFrom(blobBody)
		if n == 0 || err != nil {
			return
		}
		// only take the first line, in case the version metadata file is upgraded in the future
		remoteVersion := strings.Split(buf.String(), "\n")[0]

		// step 5: compare remote version to local version to see if there's a newer AzCopy
		v1, err := NewVersion(common.AzcopyVersion)
		if err != nil {
			return
		}
		v2, err := NewVersion(remoteVersion)
		if err != nil {
			return
		}

		if v1.OlderThan(*v2) {
			executablePathSegments := strings.Split(strings.Replace(os.Args[0], "\\", "/", -1), "/")
			executableName := executablePathSegments[len(executablePathSegments)-1]

			// output in info mode instead of stderr, as it was crashing CI jobs of some people
			glcm.Info(executableName + ": A newer version " + remoteVersion + " is available to download\n")
		}

		// let caller know we have finished, if they want to know
		close(completionChannel)
	}()

	return completionChannel
}
