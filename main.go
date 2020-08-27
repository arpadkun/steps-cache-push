// Cache Push step keeps the project's cache in sync with the project's current state based on the defined files to be cached and ignored.
//
// Files to be cached are described by a path and an optional descriptor file path.
// Files to be cached can be referred by direct file path while multiple files can be selected by referring the container directory.
// Optional indicator represents a files, based on which the step synchronizes the given file(s).
// Syntax: file/path/to/cache, dir/to/cache, file/path/to/cache -> based/on/this/file, dir/to/cache -> based/on/this/file
//
// Ignore items are used to ignore certain file(s) from a directory to be cached or to mark that certain file(s) not relevant in cache synchronization.
// Syntax: not/relevant/file/or/pattern, !file/or/pattern/to/remove/from/cache
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bitrise-io/go-utils/log"
)

const (
	cacheInfoFilePath = "/tmp/cache-info.json"
	cacheArchivePath  = "/tmp/cache-archive.tar"
	stackVersionsPath = "/tmp/archive_info.json"
)

func logErrorfAndExit(format string, args ...interface{}) {
	log.Errorf(format, args...)
	os.Exit(1)
}

/////////////////////////////////////////////
// rsync process' function
func rsyncProcess(receivedRsyncParams []string) (string, error) {

	rsynccmd := exec.Command("rsync", receivedRsyncParams...)

	var output string
	rsyncoutput, err := rsynccmd.CombinedOutput()
	if err != nil {
		output = string(rsyncoutput)
		return output, err
	}

	output = string(rsyncoutput)
	return output, nil
}

// Worker for parallel processing (running rsync on multiple threads)
func worker(id int, LocalCacheFilesListFile string, FilesForOneWorker []string, wg *sync.WaitGroup) {

	defer wg.Done()

	fmt.Printf("\n------------------------\nWorker %d starting\n", id)
	LocalCacheFilesListFileWorkerID := LocalCacheFilesListFile + "." + strconv.Itoa(id)
	fmt.Printf("Writing chunk for worker %d to: %v\n", id, LocalCacheFilesListFileWorkerID)

	// Write file list chunk to temporary sync to LocalCacheFilesListFile.worker#
	filesListFile, err := os.Create(LocalCacheFilesListFileWorkerID)
	if err != nil {
		fmt.Println(err)
	} else {
		for _, pth := range FilesForOneWorker {
			filesListFile.WriteString(string(pth) + "\n")
		}
	}
	filesListFile.Close()

	// NEED TO FIX -- restructure to remove these variables, testing only...
	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"
	LocalCacheFilesDstURL := os.Getenv("LOCAL_CACHE_DST_URL")
	LocalCacheStoragePort := "22"
	LocalCacheStoragePortTimeout := "3"

	rsyncSettingsSSHsetup := "/usr/bin/ssh -i " + LocalCacheStorageSSHKeyFile + " -o ConnectTimeout=" + LocalCacheStoragePortTimeout + " -p " + LocalCacheStoragePort
	rsyncSettingsFilesFrom := "--files-from=" + LocalCacheFilesListFileWorkerID
	rsyncSettingsDestinationURL := LocalCacheFilesDstURL

	rsyncArgs := []string{"-e", rsyncSettingsSSHsetup, rsyncSettingsFilesFrom, "--dirs", "--relative", "--archive", "--no-D", "--inplace", "--executability", "--delete", "--ignore-errors", "--force", "--compress", "--stats", "--human-readable", "--no-whole-file", "--prune-empty-dirs", "/", rsyncSettingsDestinationURL} // "--copy-dirlinks",
	fmt.Printf("DEBUG:  %v\n\n", rsyncArgs)

	// Starting the rsync process here
	output, err := rsyncProcess(rsyncArgs)
	if err != nil {
		os.Stderr.WriteString(fmt.Sprintf("==> rsync error:\n%v\n\n", err.Error()))
	}
	log.Printf("==> rsync output:\n%v\n\n", string(output))

	//fmt.Printf("#v:  %#v\n\n", FilesForOneWorker)
	//time.Sleep(time.Second)
	fmt.Printf("Worker %d done\n", id)
}

func main() {
	stepStartedAt := time.Now()

	configs, err := ParseConfig()
	if err != nil {
		logErrorfAndExit(err.Error())
	}

	configs.Print()
	fmt.Println()

	log.SetEnableDebugLog(configs.DebugMode)

	// Cleaning paths
	startTime := time.Now()

	log.Infof("Cleaning paths")

	pathToIndicatorPath := parseIncludeList(strings.Split(configs.Paths, "\n"))
	if len(pathToIndicatorPath) == 0 {
		log.Warnf("No path to cache, skip caching...")
		os.Exit(0)
	}

	pathToIndicatorPath, err = normalizeIndicatorByPath(pathToIndicatorPath)
	if err != nil {
		logErrorfAndExit("Failed to parse include list: %s", err)
	}

	excludeByPattern := parseIgnoreList(strings.Split(configs.IgnoredPaths, "\n"))
	excludeByPattern, err = normalizeExcludeByPattern(excludeByPattern)
	if err != nil {
		logErrorfAndExit("Failed to parse ignore list: %s", err)
	}

	pathToIndicatorPath = interleave(pathToIndicatorPath, excludeByPattern)

	log.Donef("Done in %s\n", time.Since(startTime))

	if len(pathToIndicatorPath) == 0 {
		log.Warnf("No path to cache, skip caching...")
		os.Exit(0)
	}

	// Check previous cache
	startTime = time.Now()

	prevDescriptor, err := readCacheDescriptor(cacheInfoFilePath)
	if err != nil {
		logErrorfAndExit("Failed to read previous cache descriptor: %s", err)
	}

	if prevDescriptor != nil {
		log.Printf("Previous cache info found at: %s", cacheInfoFilePath)
	} else {
		log.Printf("No previous cache info found")
	}

	curDescriptor, err := cacheDescriptor(pathToIndicatorPath, ChangeIndicator(configs.FingerprintMethodID))
	if err != nil {
		logErrorfAndExit("Failed to create current cache descriptor: %s", err)
	}

	log.Warnf("-----------------------------AKARMI Warnf-----------------------------")

	log.Donef("Done in %s\n", time.Since(startTime))

	// Checking file changes
	if prevDescriptor != nil {
		startTime = time.Now()

		log.Infof("Checking for file changes")

		logDebugPaths := func(paths []string) {
			for _, pth := range paths {
				log.Debugf("- %s", pth)
			}
		}

		result := compare(prevDescriptor, curDescriptor)

		log.Warnf("%d files needs to be removed", len(result.removed))
		logDebugPaths(result.removed)
		log.Warnf("%d files has changed", len(result.changed))
		logDebugPaths(result.changed)
		log.Warnf("%d files added", len(result.added))
		logDebugPaths(result.added)
		log.Debugf("%d ignored files removed", len(result.removedIgnored))
		logDebugPaths(result.removedIgnored)
		log.Debugf("%d files did not change", len(result.matching))
		logDebugPaths(result.matching)
		log.Debugf("%d ignored files added", len(result.addedIgnored))
		logDebugPaths(result.addedIgnored)

		if result.hasChanges() {
			log.Donef("File changes found in %s\n", time.Since(startTime))
		} else {
			log.Donef("No files found in %s\n", time.Since(startTime))
			log.Printf("Total time: %s", time.Since(stepStartedAt))
			os.Exit(0)
		}
	}

	// Generate cache archive
	startTime = time.Now()

	log.Infof("Generating cache archive")

	//K:
	log.Debugf("-----------------------------AKARMI debug-----------------------------")
	log.Printf("-----------------------------AKARMI Printf-----------------------------")
	log.Infof("-----------------------------AKARMI Infof-----------------------------")
	fmt.Println("-----------------------------AKARMI fmt.println-----------------------------")

	// Requirements, set up ENV Variables as secrets <-----:
	// LOCAL_CACHE_DST_URL
	// LOCAL_CACHE_KEY

	// Getting the ssh key into variable
	LocalCacheKey := os.Getenv("LOCAL_CACHE_KEY")
	LocalCacheKeyDecoded, _ := base64.URLEncoding.DecodeString(LocalCacheKey)
	numCPU, err := strconv.Atoi(os.Getenv("LOCAL_CACHE_SYNC_WORKERS"))

	// Write the ssh key to file
	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"
	LocalCacheFilesListFile := HomeDir + "/.local_cache_file_list"

	// Write ssh key file
	file, err := os.Create(LocalCacheStorageSSHKeyFile)
	if err != nil {
		fmt.Println(err)
	} else {
		file.WriteString(string(LocalCacheKeyDecoded))
	}
	file.Close()

	err = os.Chmod(LocalCacheStorageSSHKeyFile, 0600)
	if err != nil {
		fmt.Println(err)
	}

	// Write file list to sync to LocalCacheFilesListFile
	filesListFile, err := os.Create(LocalCacheFilesListFile)
	if err != nil {
		fmt.Println(err)
	} else {
		for pth := range pathToIndicatorPath {
			filesListFile.WriteString(string(pth) + "\n")
		}
		filesListFile.WriteString(string(LocalCacheFilesListFile) + "\n") // Write the file containing the file list at the end to send up to cache
	}
	filesListFile.Close()

	// Create an array of the files in mem so we can chunk them into pieces
	var FilesToSync []string
	for path := range pathToIndicatorPath {
		FilesToSync = append(FilesToSync, path)
	}
	FilesToSync = append(FilesToSync, LocalCacheFilesListFile)
	sort.Strings(FilesToSync)

	// Split up the list ~equally for parallel processing, but only if there are more than a 100 items:
	numberOfFiles := len(FilesToSync)
	if numberOfFiles == 0 {
		log.Infof("File list is empty, nothing to sync")
		return
	}

	if numberOfFiles < 100 {
		numCPU = 1
	}

	fmt.Printf("Number of files to sync: %#v\n", numberOfFiles)
	fmt.Printf("Workers to spawn: %#v\n", numCPU)

	var filesDivided [][]string
	chunkSize := (len(FilesToSync) + numCPU - 1) / numCPU
	fmt.Printf("Chunksize per worker: %#v\n", chunkSize)

	for i := 0; i < len(FilesToSync); i += chunkSize {
		end := i + chunkSize
		if end > len(FilesToSync) {
			end = len(FilesToSync)
		}

		filesDivided = append(filesDivided, FilesToSync[i:end])
	}

	/////////////////////////////////////////////////////////////////////////////////////////
	// Spin up workers
	log.Infof("Syncing files now...")

	var wg sync.WaitGroup

	for i := 0; i <= numCPU-1; i++ {
		wg.Add(1)

		go worker(i, LocalCacheFilesListFile, filesDivided[i], &wg)
	}

	wg.Wait()

	//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////
	// // OLD PATH
	// // Configuring rsync parameters
	// rsyncSettingsSSHsetup := "/usr/bin/ssh -i " + LocalCacheStorageSSHKeyFile + " -o ConnectTimeout=" + LocalCacheStoragePortTimeout + " -p " + LocalCacheStoragePort
	// rsyncSettingsFilesFrom := "--files-from=" + LocalCacheFilesListFile
	// rsyncSettingsDestinationURL := LocalCacheFilesDstURL
	// rsyncArgs := []string{"-e", rsyncSettingsSSHsetup, rsyncSettingsFilesFrom, "--dirs", "--relative", "--archive", "--no-D", "--inplace", "--executability", "--delete", "--ignore-errors", "--force", "--compress", "--stats", "--human-readable", "--no-whole-file", "--prune-empty-dirs", "/", rsyncSettingsDestinationURL}

	// //fmt.Printf("%v", rsyncArgs)

	// cmd := exec.Command("rsync", rsyncArgs...)

	// output, err := cmd.CombinedOutput()
	// if err != nil {
	// 	os.Stderr.WriteString(fmt.Sprintf("==> Error: %v\n", err.Error()))
	// }
	// log.Printf("%v\n", string(output))
	//////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////////

	// K:  cycling through the files/directories that are required to be saved
	// log.Printf("\n============================================================================================")
	// for pth := range pathToIndicatorPath {
	// 	// log.Printf("%s", pth)
	// 	// log.Printf("============================================================================================")
	// 	// log.Printf("This is in the pathToIndicatorPath variable: %s", pth)
	// 	var cmd1 = exec.Command("file", pth)
	// 	output, err := cmd1.Output()
	// 	if err != nil {
	// 		log.Printf("Could not run find, failed")
	// 	}
	// 	log.Printf("%v\n", string(output))

	// 	// // If the path is directory, let's print the contents
	// 	// if info, err := os.Stat(pth); err == nil && info.IsDir() {
	// 	// 	var cmd2 = exec.Command("find", pth)
	// 	// 	output, err := cmd2.Output()
	// 	// 	if err != nil {
	// 	// 		log.Printf("Could not run find, failed")
	// 	// 	}
	// 	// 	log.Printf("------------ Directory [%s] Contents ------------:\n  %v\n", pth, string(output))
	// 	// }
	// }

	log.Printf("============================================================================================")

	//archive, err := NewArchive(cacheArchivePath, configs.CompressArchive == "true")
	//if err != nil {
	//	logErrorfAndExit("Failed to create archive: %s", err)
	//}

	stackData, err := stackVersionData(configs.StackID)
	if err != nil {
		logErrorfAndExit("Failed to get stack version info: %s", err)
	}
	// K:
	log.Printf("EGY stackVersionData: %s", stackData)

	// This is the first file written, to speed up reading it in subsequent builds
	//if err = archive.writeData(stackData, stackVersionsPath); err != nil {
	//	logErrorfAndExit("Failed to write cache info to archive, error: %s", err)
	//}

	//if err := archive.Write(pathToIndicatorPath); err != nil {
	//	logErrorfAndExit("Failed to populate archive: %s", err)
	//}

	//if err := archive.WriteHeader(curDescriptor, cacheInfoFilePath); err != nil {
	//	logErrorfAndExit("Failed to write archive header: %s", err)
	//}

	//if err := archive.Close(); err != nil {
	//	logErrorfAndExit("Failed to close archive: %s", err)
	//}

	log.Donef("Done in %s\n", time.Since(startTime))

	// Upload cache archive
	//startTime = time.Now()

	//log.Infof("Uploading cache archive")

	//if err := uploadArchive(cacheArchivePath, configs.CacheAPIURL); err != nil {
	//	logErrorfAndExit("Failed to upload archive: %s", err)
	//}
	log.Donef("Done in %s\n", time.Since(startTime))
	log.Donef("Total time: %s", time.Since(stepStartedAt))
}
