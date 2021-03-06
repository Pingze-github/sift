// sift
// Copyright (C) 2014-2016 Sven Taute
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, version 3 of the License.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package sift

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/svent/go-flags"
	"github.com/svent/go-nbreader"
	"github.com/svent/sift/gitignore"
	"golang.org/x/crypto/ssh/terminal"
)

const (
	// InputMultilineWindow is the size of the sliding window for multiline matching
	InputMultilineWindow = 32 * 1024
	// MultilinePipeTimeout is the timeout for reading and matching input
	// from STDIN/network in multiline mode
	MultilinePipeTimeout = 1000 * time.Millisecond
	// MultilinePipeChunkTimeout is the timeout to consider last input from STDIN/network
	// as a complete chunk for multiline matching
	MultilinePipeChunkTimeout = 150 * time.Millisecond
	// MaxDirRecursionRoutines is the maximum number of parallel routines used
	// to recurse into directories
	MaxDirRecursionRoutines = 3
	SiftConfigFile          = ".sift.conf"
	SiftVersion             = "0.9.0"
)

type ConditionType int

const (
	ConditionPreceded ConditionType = iota
	ConditionFollowed
	ConditionSurrounded
	ConditionFileMatches
	ConditionLineMatches
	ConditionRangeMatches
)

type Condition struct {
	regex          *regexp.Regexp
	conditionType  ConditionType
	within         int64
	lineRangeStart int64
	lineRangeEnd   int64
	negated        bool
}

type FileType struct {
	Patterns     []string
	ShebangRegex *regexp.Regexp
}

type Match struct {
	// offset of the start of the match
	Start int64
	// offset of the end of the match
	End int64
	// offset of the beginning of the first line of the match
	LineStart int64
	// offset of the end of the last line of the match
	LineEnd int64
	// the match
	Match string
	// the match including the non-matched text on the first and last line
	Line string
	// the line number of the beginning of the match
	Lineno int64
	// the index to (*global).conditions (if this match belongs to a condition)
	ConditionID int
	// the context before the match
	ContextBefore *string
	// the context after the match
	ContextAfter *string
}

type Matches []Match

func (e Matches) Len() int           { return len(e) }
func (e Matches) Swap(i, j int)      { e[i], e[j] = e[j], e[i] }
func (e Matches) Less(i, j int) bool { return e[i].Start < e[j].Start }

type Result struct {
	ConditionMatches Matches
	Matches          Matches
	// if too many matches are found or input is read only from STDIN,
	// matches are streamed through a channel
	MatchChan chan Matches
	Streaming bool
	IsBinary  bool
	Target    string
}

var (
	InputBlockSize int = 256 * 1024
	options        Options
	errorLogger    = log.New(os.Stderr, "Error: ", 0)
	errLineTooLong = errors.New("line too long")
)

type Global struct {
	conditions            []Condition
	filesChan             chan string
	directoryChan         chan string
	fileTypesMap          map[string]FileType
	includeFilepathRegex  *regexp.Regexp
	excludeFilepathRegex  *regexp.Regexp
	netTcpRegex           *regexp.Regexp
	outputFile            io.Writer
	matchPatterns         []string
	matchRegexes          []*regexp.Regexp
	gitignoreCache        *gitignore.GitIgnoreCache
	resultsChan           chan *Result
	results				  [] *Result
	resultsDoneChan       chan struct{}
	stopChan			  chan int64
	targetsWaitGroup      sync.WaitGroup
	recurseWaitGroup      sync.WaitGroup
	streamingAllowed      bool
	streamingThreshold    int
	termHighlightFilename string
	termHighlightLineno   string
	termHighlightMatch    string
	termHighlightReset    string
	totalLineLengthErrors int64
	totalMatchCount       int64
	totalResultCount      int64
	totalTargetCount      int64
	timeCost			  time.Duration
}

//var global *Global = Global{
//	outputFile:         os.Stdout,
//	netTcpRegex:        regexp.MustCompile(`^(tcp[46]?)://(.*:\d+)$`),
//	streamingThreshold: 1 << 16,
//}

// 返回数据
type SearchResult struct {
	Results 	[] *Result
	TimeCost	time.Duration
}

// processDirectories reads (*global).directoryChan and processes
// directories via processDirectory.
func processDirectories(global *Global) {
	n := options.Cores
	if n > MaxDirRecursionRoutines {
		n = MaxDirRecursionRoutines
	}
	for i := 0; i < n; i++ {
		go func() {
			for dirname := range (*global).directoryChan {
				processDirectory(dirname, global)
			}
		}()
	}
}

// enqueueDirectory enqueues directories on (*global).directoryChan.
// If the channel blocks, the directory is processed directly.
func enqueueDirectory(dirname string, global *Global) {
	(*global).recurseWaitGroup.Add(1)
	select {
	case (*global).directoryChan <- dirname:
	default:
		processDirectory(dirname, global)
	}
}

// processDirectory recurses into a directory and sends all files
// fulfilling the selected options on (*global).filesChan
func processDirectory(dirname string, global *Global) {
	defer (*global).recurseWaitGroup.Done()
	var gic *gitignore.Checker
	if options.Git {
		gic = gitignore.NewCheckerWithCache((*global).gitignoreCache)
		err := gic.LoadBasePath(dirname)
		if err != nil {
			errorLogger.Printf("cannot load gitignore files for path '%s': %s", dirname, err)
		}
	}
	dir, err := os.Open(dirname)
	if err != nil {
		errorLogger.Printf("cannot open directory '%s': %s\n", dirname, err)
		return
	}
	defer dir.Close()
	for {
		entries, err := dir.Readdir(256)
		if err == io.EOF {
			return
		}
		if err != nil {
			errorLogger.Printf("cannot read directory '%s': %s\n", dirname, err)
			return
		}

	nextEntry:
		for _, fi := range entries {
			fullpath := filepath.Join(dirname, fi.Name())

			// check directory include/exclude options
			if fi.IsDir() {
				if !options.Recursive {
					continue nextEntry
				}
				for _, dirPattern := range options.ExcludeDirs {
					matched, err := filepath.Match(dirPattern, fi.Name())
					if err != nil {
						errorLogger.Fatalf("cannot match malformed pattern '%s' against directory name: %s\n", dirPattern, err)
					}
					if matched {
						continue nextEntry
					}
				}
				if len(options.IncludeDirs) > 0 {
					for _, dirPattern := range options.IncludeDirs {
						matched, err := filepath.Match(dirPattern, fi.Name())
						if err != nil {
							errorLogger.Fatalf("cannot match malformed pattern '%s' against directory name: %s\n", dirPattern, err)
						}
						if matched {
							goto includeDirMatchFound
						}
					}
					continue nextEntry
				includeDirMatchFound:
				}
				if options.Git {
					if fi.Name() == gitignore.GitFoldername || gic.Check(fullpath, fi) {
						continue nextEntry
					}
				}
				enqueueDirectory(fullpath, global)
				continue nextEntry
			}

			// check whether this is a regular file
			if fi.Mode()&os.ModeType != 0 {
				if options.FollowSymlinks && fi.Mode()&os.ModeType == os.ModeSymlink {
					realPath, err := filepath.EvalSymlinks(fullpath)
					if err != nil {
						errorLogger.Printf("cannot follow symlink '%s': %s\n", fullpath, err)
					} else {
						realFi, err := os.Stat(realPath)
						if err != nil {
							errorLogger.Printf("cannot follow symlink '%s': %s\n", fullpath, err)
						}
						if realFi.IsDir() {
							enqueueDirectory(realPath, global)
							continue nextEntry
						} else {
							if realFi.Mode()&os.ModeType != 0 {
								continue nextEntry
							}
						}
					}
				} else {
					continue nextEntry
				}
			}

			// check file path options
			if (*global).excludeFilepathRegex != nil {
				if (*global).excludeFilepathRegex.MatchString(fullpath) {
					continue nextEntry
				}
			}
			if (*global).includeFilepathRegex != nil {
				if !(*global).includeFilepathRegex.MatchString(fullpath) {
					continue nextEntry
				}
			}

			// check file extension options
			if len(options.ExcludeExtensions) > 0 {
				for _, e := range strings.Split(options.ExcludeExtensions, ",") {
					if filepath.Ext(fi.Name()) == "."+e {
						continue nextEntry
					}
				}
			}
			if len(options.IncludeExtensions) > 0 {
				for _, e := range strings.Split(options.IncludeExtensions, ",") {
					if filepath.Ext(fi.Name()) == "."+e {
						goto includeExtensionFound
					}
				}
				continue nextEntry
			includeExtensionFound:
			}

			// check file include/exclude options
			for _, filePattern := range options.ExcludeFiles {
				matched, err := filepath.Match(filePattern, fi.Name())
				if err != nil {
					errorLogger.Fatalf("cannot match malformed pattern '%s' against file name: %s\n", filePattern, err)
				}
				if matched {
					continue nextEntry
				}
			}
			if len(options.IncludeFiles) > 0 {
				for _, filePattern := range options.IncludeFiles {
					matched, err := filepath.Match(filePattern, fi.Name())
					if err != nil {
						errorLogger.Fatalf("cannot match malformed pattern '%s' against file name: %s\n", filePattern, err)
					}
					if matched {
						goto includeFileMatchFound
					}
				}
				continue nextEntry
			includeFileMatchFound:
			}

			// check file type options
			if len(options.ExcludeTypes) > 0 {
				for _, t := range strings.Split(options.ExcludeTypes, ",") {
					for _, filePattern := range (*global).fileTypesMap[t].Patterns {
						if matched, _ := filepath.Match(filePattern, fi.Name()); matched {
							continue nextEntry
						}
					}
					sr := (*global).fileTypesMap[t].ShebangRegex
					if sr != nil {
						if m, err := checkShebang((*global).fileTypesMap[t].ShebangRegex, fullpath); m && err == nil {
							continue nextEntry
						}
					}
				}
			}
			if len(options.IncludeTypes) > 0 {
				for _, t := range strings.Split(options.IncludeTypes, ",") {
					for _, filePattern := range (*global).fileTypesMap[t].Patterns {
						if matched, _ := filepath.Match(filePattern, fi.Name()); matched {
							goto includeTypeFound
						}
					}
					sr := (*global).fileTypesMap[t].ShebangRegex
					if sr != nil {
						if m, err := checkShebang((*global).fileTypesMap[t].ShebangRegex, fullpath); err != nil || m {
							goto includeTypeFound
						}
					}
				}
				continue nextEntry
			includeTypeFound:
			}

			if options.Git {
				if fi.Name() == gitignore.GitIgnoreFilename || gic.Check(fullpath, fi) {
					continue
				}
			}

			(*global).filesChan <- fullpath
		}
	}
}

// checkShebang checks whether the first line of file matches the given regex
func checkShebang(regex *regexp.Regexp, filepath string) (bool, error) {
	f, err := os.Open(filepath)
	defer f.Close()
	if err != nil {
		return false, err
	}
	b, err := bufio.NewReader(f).ReadBytes('\n')
	return regex.Match(b), nil
}

// processFileTargets reads filesChan, builds an io.Reader for the target and calls processReader
// 关键 文件搜索
// 每个线程执行一次
func processFileTargets(global *Global) {

	// 控制等待组完成
	defer (*global).targetsWaitGroup.Done()

	dataBuffer := make([]byte, InputBlockSize)
	testBuffer := make([]byte, InputBlockSize)
	matchRegexes := make([]*regexp.Regexp, len((*global).matchPatterns))
	for i := range (*global).matchPatterns {
		matchRegexes[i] = regexp.MustCompile((*global).matchPatterns[i])
	}

	for {
		select {
			case <- (*global).stopChan:
				return
			default:
				filePath := <-(*global).filesChan
				if filePath == "" {
					return
				}
				var err error
				var infile *os.File
				var reader io.Reader

				if options.TargetsOnly {
				  (*global).resultsChan <- &Result{Target: filePath}
				  continue
				}

				// 读取文件为infile
				if filePath == "-" {
				  infile = os.Stdin
				} else {

				  infile, err = os.Open(filePath)
				  if err != nil {
					  errorLogger.Printf("cannot open file '%s': %s\n", filePath, err)
					  continue
				  }
				}

				if options.Zip && strings.HasSuffix(filePath, ".gz") {
				  rawReader := infile
				  reader, err = gzip.NewReader(rawReader)
				  if err != nil {
					  errorLogger.Printf("error decompressing file '%s', opening as normal file\n", infile.Name())
					  infile.Seek(0, 0)
					  reader = infile
				  }
				} else if infile == os.Stdin && options.Multiline {
				  reader = nbreader.NewNBReader(infile, InputBlockSize,
					  nbreader.ChunkTimeout(MultilinePipeChunkTimeout), nbreader.Timeout(MultilinePipeTimeout))
				} else {
				  // 正常进入这个分支
				  reader = infile
				}

				if options.InvertMatch {
				  err = processReaderInvertMatch(reader, matchRegexes, filePath, global)
				} else {
				  // 正常进入这个分支
				  // reader 文件reader io.Reader
				  // matchRegexes 正则s
				  // dataBuffer 结果buffer
				  // testBuffer 结果buffer
				  // testBuffer 结果buffer
				  // filePath 文件路径
				  err = processReader(reader, matchRegexes, dataBuffer, testBuffer, filePath, global)
				}
				if err != nil {
				  if err == errLineTooLong {
					  (*global).totalLineLengthErrors += 1
					  if options.ErrShowLineLength {
						  errmsg := fmt.Sprintf("file contains very long lines (>= %d bytes). See options --blocksize and --err-skip-line-length.", InputBlockSize)
						  errorLogger.Printf("cannot process data from file '%s': %s\n", filePath, errmsg)
					  }
				  } else {
					  errorLogger.Printf("cannot process data from file '%s': %s\n", filePath, err)
				  }
				}
				infile.Close()
		}
	}
}

// processNetworkTarget starts a listening TCP socket and calls processReader
func processNetworkTarget(target string, global *Global) {
	matchRegexes := make([]*regexp.Regexp, len((*global).matchPatterns))
	for i := range (*global).matchPatterns {
		matchRegexes[i] = regexp.MustCompile((*global).matchPatterns[i])
	}
	defer (*global).targetsWaitGroup.Done()

	var reader io.Reader
	netParams := (*global).netTcpRegex.FindStringSubmatch(target)
	proto := netParams[1]
	addr := netParams[2]

	listener, err := net.Listen(proto, addr)
	if err != nil {
		errorLogger.Fatalf("could not listen on '%s'\n", target)
	}

	conn, err := listener.Accept()
	if err != nil {
		errorLogger.Fatalf("could not accept connections on '%s'\n", target)
	}

	if options.Multiline {
		reader = nbreader.NewNBReader(conn, InputBlockSize, nbreader.ChunkTimeout(MultilinePipeChunkTimeout),
			nbreader.Timeout(MultilinePipeTimeout))
	} else {
		reader = conn
	}

	dataBuffer := make([]byte, InputBlockSize)
	testBuffer := make([]byte, InputBlockSize)
	err = processReader(reader, matchRegexes, dataBuffer, testBuffer, target, global)
	if err != nil {
		errorLogger.Printf("error processing data from '%s'\n", target)
		return
	}
}

// 结果收集
func resultCollector(global *Global) {
	for result := range (*global).resultsChan {
		if options.TargetsOnly {
			continue
		}
		(*global).totalTargetCount++
		result.applyConditions(global)
		if len(result.Matches) > 0 {
			(*global).results = append((*global).results, result)
		}
	}
	(*global).resultsDoneChan <- struct{}{}
}

// 执行搜索
func executeSearch(targets []string, global *Global) (ret int, err error) {
	defer func() {
		if r := recover(); r != nil {
			ret = 2
			return
		}
	}()
	tstart := time.Now()
	(*global).filesChan = make(chan string, 256)
	(*global).directoryChan = make(chan string, 128)
	(*global).resultsChan = make(chan *Result, 128)
	(*global).resultsDoneChan = make(chan struct{})
	(*global).gitignoreCache = gitignore.NewGitIgnoreCache()
	(*global).totalTargetCount = 0
	(*global).totalLineLengthErrors = 0
	(*global).totalMatchCount = 0
	(*global).totalResultCount = 0

	// 关键：结果处理
	// go resultHandler()
	go resultCollector(global)

	for i := 0; i < options.Cores; i++ {
		(*global).targetsWaitGroup.Add(1)
		// 关键：文件目标处理
		go processFileTargets(global)
	}

	// 关键：目录目标处理
	go processDirectories(global)

	for _, target := range targets {
		switch {
		case target == "-":
			(*global).filesChan <- "-"
		case (*global).netTcpRegex.MatchString(target):
			(*global).targetsWaitGroup.Add(1)
			go processNetworkTarget(target, global)
		default:
			// 进入这里
			fileinfo, err := os.Stat(target)
			if err != nil {
				if os.IsNotExist(err) {
					errorLogger.Fatalf("no such file or directory: %s\n", target)
				} else {
					errorLogger.Fatalf("cannot open file or directory: %s\n", target)
				}
			}
			if fileinfo.IsDir() {
				// 目录搜索
				(*global).recurseWaitGroup.Add(1)
				(*global).directoryChan <- target
			} else {
				// 文件搜索
				// 将目标文件送入 filesChan

				(*global).filesChan <- target
			}
		}
	}

	(*global).recurseWaitGroup.Wait()
	close((*global).directoryChan)
	close((*global).filesChan)

	(*global).targetsWaitGroup.Wait()
	close((*global).resultsChan)
	<-(*global).resultsDoneChan

	var retVal int
	if (*global).totalResultCount > 0 {
		retVal = 0
	} else {
		retVal = 1
	}

	if !options.ErrSkipLineLength && !options.ErrShowLineLength && (*global).totalLineLengthErrors > 0 {
		errorLogger.Printf("%d files skipped due to very long lines (>= %d bytes). See options --blocksize, --err-show-line-length and --err-skip-line-length.", (*global).totalLineLengthErrors, InputBlockSize)
	}

	// 统计运算时间
	tend := time.Now()
	(*global).timeCost = tend.Sub(tstart)

	return retVal, nil
}


// 执行sift命令。可以传入sift执行命令（参数部分），来执行操作并返回结果
// TODO 超时控制 超时后，将通道close
func ExecuteSiftCmd (cmd string, timeout time.Duration) (SearchResult, error) {

	// 重新初始化
	global := &Global{
		outputFile:         os.Stdout,
		netTcpRegex:        regexp.MustCompile(`^(tcp[46]?)://(.*:\d+)$`),
		streamingThreshold: 1 << 16,
		stopChan: make(chan int64),
	}

	initFileTypes(global)

	var targets []string
	var args []string
	var err error

	cmdArgs := strings.Split(cmd, " ")

	// 通过parser解析直接将配置传递到Options
	parser := flags.NewNamedParser("sift", flags.HelpFlag|flags.PassDoubleDash)
	parser.AddGroup("Options", "Options", &options)
	parser.Name = "sift"
	parser.Usage = "[OPTIONS] PATTERN [FILE|PATH|tcp://HOST:PORT]...\n" +
		"  sift [OPTIONS] [-e PATTERN | -f FILE] [FILE|PATH|tcp://HOST:PORT]...\n" +
		"  sift [OPTIONS] --targets [FILE|PATH]..."

	// temporarily parse options to see if the --no-conf/--conf options were used and
	// then discard the result
	options.LoadDefaults()
	args, err = parser.ParseArgs(cmdArgs)

	if err != nil {
		return SearchResult{}, err
	}
	noConf := options.NoConfig
	configFile := options.ConfigFile
	options = Options{}

	// perform full option parsing respecting the --no-conf/--conf options

	// 两行做了初始化配置
	options.LoadDefaults()
	options.LoadConfigs(noConf, configFile)

	// 解析出args=[.]
	// 这个过程使options被输入参数所配置
	args, err = parser.ParseArgs(cmdArgs)
	if err != nil {
		return SearchResult{}, err
		//errorLogger.Println(err)
		//os.Exit(2)
	}

	// 整理patterns
	for _, pattern := range options.Patterns {
		(*global).matchPatterns = append((*global).matchPatterns, pattern)
	}

	// 未进入
	if options.PatternFile != "" {
		f, err := os.Open(options.PatternFile)
		if err != nil {
			errorLogger.Fatalln("Cannot open pattern file:\n", err)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			pattern := scanner.Text()
			(*global).matchPatterns = append((*global).matchPatterns, pattern)

		}
	}
	if len((*global).matchPatterns) == 0 {
		if len(args) == 0 && !(options.PrintConfig || options.WriteConfig ||
			options.TargetsOnly || options.ListTypes) {
			errorLogger.Fatalln("No pattern given. Try 'sift --help' for more information.")
		}
		if len(args) > 0 && !options.TargetsOnly {
			(*global).matchPatterns = append((*global).matchPatterns, args[0])
			args = args[1:len(args)]
		}
	}

	if len(args) == 0 {
		// check whether there is input on STDIN
		if !terminal.IsTerminal(int(os.Stdin.Fd())) {
			targets = []string{"-"}
		} else {
			targets = []string{"."}
		}
	} else {
		targets = args[0:len(args)]
	}

	// expand arguments containing patterns on Windows
	if runtime.GOOS == "windows" {
		targetsExpanded := []string{}
		for _, t := range targets {
			if t == "-" {
				targetsExpanded = append(targetsExpanded, t)
				continue
			}
			expanded, err := filepath.Glob(t)
			if err == filepath.ErrBadPattern {
				errorLogger.Fatalf("cannot parse argument '%s': %s\n", t, err)
			}
			if expanded != nil {
				for _, e := range expanded {
					targetsExpanded = append(targetsExpanded, e)
				}
			}
		}
		targets = targetsExpanded
	}

	// options 使用用户配置
	if err := options.Apply((*global).matchPatterns, targets, global); err != nil {
		errorLogger.Fatalf("cannot process options: %s\n", err)
	}

	(*global).matchRegexes = make([]*regexp.Regexp, len((*global).matchPatterns))
	for i := range (*global).matchPatterns {
		(*global).matchRegexes[i], err = regexp.Compile((*global).matchPatterns[i])
		if err != nil {
			errorLogger.Fatalf("cannot parse pattern: %s\n", err)
		}
	}

	execChan := make(chan error)

	go func() {
		_, err := executeSearch(targets, global)
		execChan <- err
	}()

	select {
	case err := <-execChan:
		if err != nil {
			errorLogger.Println(err)
		}
		return SearchResult{
			Results:(*global).results,
			TimeCost:(*global).timeCost,
		}, nil

	case <-time.After(timeout):
		// 超时
		for i := 0; i < options.Cores; i++ {
			(*global).stopChan <- 1
		}
		return SearchResult{}, errors.New(fmt.Sprintf("sift search timeout for %s limit", timeout))
	}
}

// 为了使匹配结果可以在包外获取，需对结果集进行转换

// 测试方法
func Test () {
	searchResult, _ := ExecuteSiftCmd("-e sift . -n", time.Duration(1e9))
	for _, result := range(searchResult.Results) {
		fmt.Println("这是一个文件的搜索结果：")
		fmt.Println(result)
	}
	fmt.Println("运算耗时", searchResult.TimeCost)
}