package updatercore

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
)

func HashFile(fileName string) (int64, string, string, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return 0, "", "", err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, "", "", err
	}
	size := info.Size()
	if size < sampleThreshold {
		hash, err := hashReader(file)
		if err != nil {
			return 0, "", "", err
		}
		return size, hash, hash, nil
	}

	head, err := hashSection(file, 0, sampleSize)
	if err != nil {
		return 0, "", "", err
	}
	tail, err := hashSection(file, size-sampleSize, sampleSize)
	if err != nil {
		return 0, "", "", err
	}
	return size, head, tail, nil
}

func VerifyFile(fileName string, entry FileEntry) (bool, error) {
	size, head, tail, err := HashFile(fileName)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return size == entry.Size && head == entry.HeadHash && tail == entry.TailHash, nil
}

func fileEntryForPath(rootDir, fileName string) (FileEntry, error) {
	rel, err := filepathRelSlash(rootDir, fileName)
	if err != nil {
		return FileEntry{}, err
	}
	size, head, tail, err := HashFile(fileName)
	if err != nil {
		return FileEntry{}, err
	}
	return FileEntry{Path: rel, Size: size, HeadHash: head, TailHash: tail}, nil
}

func hashReader(reader io.Reader) (string, error) {
	hash := sha256.New()
	if _, err := io.Copy(hash, reader); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashSection(file *os.File, offset int64, length int64) (string, error) {
	reader := io.NewSectionReader(file, offset, length)
	return hashReader(reader)
}
