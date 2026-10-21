package sessionstats

import "os"

type fileStat struct {
	mtimeMS int64
	size    int64
}

func statFile(file string) (fileStat, error) {
	info, err := os.Stat(file)
	if err != nil {
		return fileStat{}, err
	}
	return fileStat{mtimeMS: info.ModTime().UnixMilli(), size: info.Size()}, nil
}
