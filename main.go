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
	"path/filepath"
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

	rsynccmd := exec.Command("time", receivedRsyncParams...)

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

	rsyncArgs := []string{"rsync", "-e", rsyncSettingsSSHsetup, rsyncSettingsFilesFrom, "--dirs", "--relative", "--archive", "--no-D", "--inplace", "--executability", "--delete", "--ignore-errors", "--force", "--compress", "--stats", "--human-readable", "--no-whole-file", "--prune-empty-dirs", "/", rsyncSettingsDestinationURL} // "--copy-dirlinks",
	//fmt.Printf("DEBUG:  %v\n\n", rsyncArgs)

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

// Walking through directory
func filePathWalkDir(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	return files, err
}

// failf prints an error and terminates the step.
func failf(format string, args ...interface{}) {
	log.Errorf(format, args...)
	os.Exit(1)
}

func getFileSize(filepath string) (int64, error) {
	fi, err := os.Stat(filepath)
	if err != nil {
		return 0, err
	}
	// get the size
	return fi.Size(), nil
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

	DirWithLargeFiles := parseIncludeList(strings.Split(configs.DirWithLargeFiles, "\n"))

	mergeIgnoredPathsAndDirWithLargeFiles := strings.Split(configs.IgnoredPaths, "\n")

	// Adding contents (Dir PATHs) of DirWithLargeFiles to IgnoredPaths, with correct syntax to exclude them from the list (we copy them differently)
	for addthis := range DirWithLargeFiles {
		var add string
		add = "!" + addthis + "/**"
		mergeIgnoredPathsAndDirWithLargeFiles = append(mergeIgnoredPathsAndDirWithLargeFiles, add)
	}

	fmt.Printf("EXCLUDE: %#v\n\n", mergeIgnoredPathsAndDirWithLargeFiles)

	//excludeByPattern := parseIgnoreList(strings.Split(configs.IgnoredPaths, "\n"))
	excludeByPattern := parseIgnoreList(mergeIgnoredPathsAndDirWithLargeFiles)

	for lo := range excludeByPattern {
		fmt.Printf("excludeByPattern BEFORE NORMALIZE =>  %s\n", lo)
	}

	excludeByPattern, err = normalizeExcludeByPattern(excludeByPattern)
	if err != nil {
		logErrorfAndExit("Failed to parse ignore list: %s", err)
	}

	for la := range excludeByPattern {
		fmt.Printf("excludeByPattern =>  %s\n", la)
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

	if len(os.Getenv("LOCAL_CACHE_KEY")) < 1000 {
		failf("ERROR: missing or invalid required environment variable:  LOCAL_CACHE_KEY")
	}

	if len(os.Getenv("LOCAL_CACHE_DST_URL")) < 15 {
		failf("ERROR: missing or invalid required environment variable:  LOCAL_CACHE_DST_URL")
	}

	// Getting the ssh key into variable
	LocalCacheKey := os.Getenv("LOCAL_CACHE_KEY")
	LocalCacheKeyDecoded, _ := base64.URLEncoding.DecodeString(LocalCacheKey)

	var numCPU int
	if len(os.Getenv("LOCAL_CACHE_SYNC_WORKERS")) == 0 {
		numCPU = 6 // Default for parallel workers if ENV variable is missing
	} else {
		numCPU, err = strconv.Atoi(os.Getenv("LOCAL_CACHE_SYNC_WORKERS"))
	}

	// Write the ssh key to file
	HomeDir := os.Getenv("HOME")
	LocalCacheStorageSSHKeyFile := HomeDir + "/.ssh/local_cache.key"
	LocalCacheFilesListDir := HomeDir + "/.local_cache_xfer_lists"                      // Dir - file lists go into this directory (control plane)
	LocalCacheFilesListFile := LocalCacheFilesListDir + "/local_cache_file_list"        // File - list of all files to transfer
	LocalCacheLargeFilesDirXferList := LocalCacheFilesListDir + "/chunked_dir_list"     // File - list containing which directories were tar'ed and chunked up
	LocalCacheLargeFilesDirXferChunks := LocalCacheFilesListDir + "/chunked_dir_chunks" // File - list containing the split tar file names, like xaa, xab, etc... Using this to send up what needs to be pulled
	LocalCacheLargeFilesDirXferDir := HomeDir + "/.local_cache_xfer"                    // Dir - for large file chunks that are split up (data plane)

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

	// Cleaning and re-Create Xfer List directory & Xfer dir
	_ = os.RemoveAll(LocalCacheFilesListDir)
	_ = os.Mkdir(LocalCacheFilesListDir, 0755)
	_ = os.RemoveAll(LocalCacheLargeFilesDirXferDir)
	_ = os.Mkdir(LocalCacheLargeFilesDirXferDir, 0755)

	var xferChunkFileList []string // Will use this to collect all the chunk file names and add to the list to rsync to copy
	// Checking if Directories with large files is set and if yes, packaging up and splitting them
	if len(DirWithLargeFiles) == 0 {
		log.Warnf("No directory with large files defined (this is fine), continuing...")
	} else {
		// Create Xfer directory
		_ = os.Mkdir(LocalCacheLargeFilesDirXferDir, 0755)
		// Write list of directories to transfer list, so we can use this to reconstruct during pull
		xferList, err := os.Create(LocalCacheLargeFilesDirXferList)
		if err != nil {
			fmt.Println(err)
		} else {
			for packThisDir := range DirWithLargeFiles { // Starting processing the directories here:
				log.Printf("Processing directory with large files: %s", packThisDir)

				archiveDir := LocalCacheLargeFilesDirXferDir + packThisDir
				archiveName := LocalCacheLargeFilesDirXferDir + packThisDir + "/for-xfer.tar"

				// Create temp container dir for xfer
				err = os.MkdirAll(archiveDir, 0755)
				if err != nil {
					fmt.Println(err)
				}

				// tar -lcf ~/.ssh/akarmi.tar /Users/kharpeet/Local_Workdir/kubernetes/.git
				tarDirArgs := []string{"tar", "-lcf", archiveName, packThisDir}
				//fmt.Printf("ARGS:  %v\n", tarDirArgs)
				tarDir := exec.Command("time", tarDirArgs...) // Running tar process here
				tarDiroutput, err := tarDir.CombinedOutput()
				if err != nil {
					os.Stderr.WriteString(fmt.Sprintf("==> Error: %v\n", err.Error()))
				}
				log.Printf("%v\n", string(tarDiroutput))

				// Get the size of the created archive so we can chunk it accordingly
				size, err := getFileSize(archiveName)
				if err != nil {
					fmt.Println(err)
				}

				var tarChunkSizePerCPU int64
				// If the file is smaller than 50MB, don't chunk up
				if size < 52428800 { // 50MB = 50*1024*1024
					log.Warnf("Large Directory Archive size is under 50MB, is this okay? Skipping chunking archive will move in a single file, continue...")
					tarChunkSizePerCPU = size
				} else {
					tarChunkSizePerCPU = (size / int64(numCPU)) + 1 // +1 byte to make sure there is no small remainder file at the end
				}

				sizeGB := float64(size) / 1073741824 // (1024.0 * 1024.0 * 1024.0)
				tarChunkSizePerCPUGB := float64(tarChunkSizePerCPU) / 1073741824
				fmt.Printf("DEBUG:  RAW SIZE: %v, CHUNKSIZE: %v\n", size, tarChunkSizePerCPU)

				fmt.Printf("Archive to move (tar):  %.4f GB, Chunk size per worker: %.4f\n", sizeGB, tarChunkSizePerCPUGB)

				// Split the tar in 100MB chunks
				// split -b 100m .local_cache_xfer/Users/kharpeet/Local_Workdir/kubernetes/.git/for-xfer.tar .local_cache_xfer/Users/kharpeet/Local_Workdir/kubernetes/.git/x
				archiveSplittedFilesPattern := archiveDir + "/x"
				tarChunkSizePerCPUstring := strconv.FormatInt(tarChunkSizePerCPU, 10)
				splitTarArgs := []string{"split", "-b", tarChunkSizePerCPUstring, archiveName, archiveSplittedFilesPattern}
				//fmt.Printf("ARGS:  %v\n", splitTarArgs)
				splitTar := exec.Command("time", splitTarArgs...) // Running tar process here
				splitTaroutput, err := splitTar.CombinedOutput()
				if err != nil {
					os.Stderr.WriteString(fmt.Sprintf("==> Error: %v\n", err.Error()))
				}
				log.Printf("%v\n", string(splitTaroutput))

				// Clean up, remove tar
				e := os.Remove(archiveName)
				if e != nil {
					fmt.Println(e)
				}

				// Collecting the file names for the chunk files in an array to add it later to the rsync list
				chunkFiles, err := filePathWalkDir(archiveDir)
				if err != nil {
					panic(err)
				}
				for _, chunkfile := range chunkFiles {
					xferChunkFileList = append(xferChunkFileList, chunkfile) // This is the array storing the chunk list to use outside this loop
				}

				// Writing the processed dir to the xferList in the end
				xferList.WriteString(string(packThisDir) + "\n")
			}
		}
		xferList.Close()
	}

	// Write chunk list to file (LocalCacheLargeFilesDirXferChunks)
	writeChunkFile, err := os.Create(LocalCacheLargeFilesDirXferChunks)
	if err != nil {
		fmt.Println(err)
	} else {
		for _, onechunk := range xferChunkFileList {
			writeChunkFile.WriteString(string(onechunk) + "\n")
		}

	}
	writeChunkFile.Close()

	// Write file list to sync to LocalCacheFilesListFile
	// 1: Write all the files that come from the User via ENV var
	// 2: Write the lists that were generated at the end to make sure they all make it to the cache:
	//      - LocalCacheFilesListFile = contains all the files to carry
	// 		- LocalCacheLargeFilesDirXferList = contains the list of directories that were chunked up, typically coming from DirWithLargeFiles variable (from ENV Var what Customer wants to chunk up)
	var FilesToSync []string                                 // This array is used later to create the chunks
	filesListFile, err := os.Create(LocalCacheFilesListFile) // Writing the same list to file
	if err != nil {
		fmt.Println(err)
	} else {
		for pth := range pathToIndicatorPath { // Cycling through the list coming from the original code
			filesListFile.WriteString(string(pth) + "\n") // Write to file here
			FilesToSync = append(FilesToSync, pth)        // Create(add) to the array of the files in mem so we can chunk them into pieces
		}

		filesListFile.WriteString(string(LocalCacheFilesListFile) + "\n") // Write the file containing the file list at the end to send up to cache // CONTROL PLANE
		FilesToSync = append(FilesToSync, LocalCacheFilesListFile)

		filesListFile.WriteString(string(LocalCacheLargeFilesDirXferList) + "\n") // Write the file containing the list of directories what we CHUNKED up, we need that list at the end to send up to cache so we can reconstruct on pull // CONTROL PLANE
		FilesToSync = append(FilesToSync, LocalCacheLargeFilesDirXferList)

		filesListFile.WriteString(string(LocalCacheLargeFilesDirXferChunks) + "\n") // Write the file containing the list chunk files (xaa, xab...) we need that list at the end to send up to cache so we can reconstruct on pull // DATA PLANE
		FilesToSync = append(FilesToSync, LocalCacheLargeFilesDirXferChunks)

	}
	filesListFile.Close()

	// for la := range FilesToSync {
	// 	fmt.Println(FilesToSync[la])
	// }
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

	// Distribute the Chunked archive pieces equally across the numCPU workers, so rsync can pull them parallel
	var numCPUCounter int
	numCPUCounter = 0
	numOfChunkedFiles := len(xferChunkFileList)
	fmt.Printf("Number of Chunked Files:  %v\n", numOfChunkedFiles)
	for e := 0; e <= numOfChunkedFiles-1; e++ { // Cycling through all files in the chunk list
		fmt.Printf("Worker %v gets chunkfile:  %v (%v/%v)\n", numCPUCounter, xferChunkFileList[e], e+1, numOfChunkedFiles)

		// filesDivided[numCPUCounter] => []string  , xferChunkFileList[e] =>  string, convertToArray =>  []string
		filesDivided[numCPUCounter] = append(filesDivided[numCPUCounter], xferChunkFileList[e]) // Distributing chunk files into the filesDivided arrays
		//fmt.Printf("filesDivided[numCPUCounter] =>  %#v\n\n", filesDivided[numCPUCounter])
		//fmt.Printf("filesDivided[numCPUCounter] => %T  , xferChunkFileList[e] =>  %T, convertToArray =>  %T\n", filesDivided[numCPUCounter], xferChunkFileList[e], convertToArray)
		// fmt.Printf("xferChunkFileList:  %#v\n\n", xferChunkFileList)
		if numCPUCounter == numCPU-1 {
			numCPUCounter = -1 // Once reached the max numCPU count, reset to the first array
		}
		numCPUCounter++
	}

	// for chunkpthid := range xferChunkFileList {
	// 	fmt.Printf("CHUNK FILE:  %v\n", xferChunkFileList[chunkpthid])

	// index:
	// 	//filesListFile.WriteString(string(xferChunkFileList[chunkpthid]) + "\n")
	// 	//FilesToSync = append(FilesToSync, xferChunkFileList[chunkpthid])
	// }

	/////////////////////////////////////////////////////////////////////////////////////////
	// Spin up workers
	log.Infof("Syncing files now...")

	var wg sync.WaitGroup

	for i := 0; i <= numCPU-1; i++ {
		wg.Add(1)

		go worker(i, LocalCacheFilesListFile, filesDivided[i], &wg)
	}

	wg.Wait()

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
