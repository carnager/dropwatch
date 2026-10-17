// Package dump reads MusicBrainz JSONL dumps without a fixed record-size limit.
package dump

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"

	"dropwatch/internal/model"
)

// ReadGroups keeps only tracked artist credits in memory. Parsing or tar errors
// reject the entire snapshot so a partial download cannot become a baseline.
func ReadGroups(ctx context.Context, path string, tracked map[string]bool, progress func(int)) (map[string][]model.ReleaseGroup, error) {
	cmd := exec.CommandContext(ctx, "tar", "-xJOf", path, "--wildcards", "mbdump/*")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting tar (tar and xz are required): %w", err)
	}
	groups, err := readGroups(ctx, stdout, tracked, progress)
	if err != nil {
		_ = cmd.Process.Kill()
	}
	closeErr := cmd.Wait()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, fmt.Errorf("extracting dump: %w", closeErr)
	}
	return groups, nil
}

func readGroups(ctx context.Context, input io.Reader, tracked map[string]bool, progress func(int)) (map[string][]model.ReleaseGroup, error) {
	reader := bufio.NewReaderSize(input, 1<<20)
	groups := make(map[string][]model.ReleaseGroup)
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, readErr
		}
		if len(bytes.TrimSpace(line)) > 0 {
			var record struct {
				ID               string   `json:"id"`
				Title            string   `json:"title"`
				PrimaryType      string   `json:"primary-type"`
				SecondaryTypes   []string `json:"secondary-types"`
				FirstReleaseDate string   `json:"first-release-date"`
				ArtistCredit     []struct {
					Artist struct{ ID string } `json:"artist"`
				} `json:"artist-credit"`
			}
			if err := json.Unmarshal(line, &record); err != nil {
				return nil, fmt.Errorf("record %d: %w", count+1, err)
			}
			if record.ID == "" || len(record.ArtistCredit) == 0 {
				return nil, fmt.Errorf("record %d is not a release group", count+1)
			}
			count++
			seen := map[string]bool{}
			for _, credit := range record.ArtistCredit {
				id := credit.Artist.ID
				if !tracked[id] || seen[id] {
					continue
				}
				seen[id] = true
				pt := record.PrimaryType
				if pt == "" {
					pt = "Other"
				}
				groups[id] = append(groups[id], model.ReleaseGroup{
					ID: record.ID, ArtistID: id, Title: record.Title,
					NormTitle: model.NormalizeTitle(record.Title), PrimaryType: pt,
					SecondaryTypes: record.SecondaryTypes, FirstReleaseDate: record.FirstReleaseDate,
					MBID: record.ID, Sources: []string{"musicbrainz"},
				})
			}
			if progress != nil && count%250000 == 0 {
				progress(count)
			}
		}
		if readErr == io.EOF {
			break
		}
	}
	if count == 0 {
		return nil, fmt.Errorf("release-group dump is empty")
	}
	return groups, nil
}
