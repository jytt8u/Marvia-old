package site_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestEveryPictureOnTheSiteIsCopiedByTheBuild: сборка Pages кладёт в shots/
// только то, что лежит в docs/shots. Картинка из другого места на сайте
// вышла бы битой, и узнали бы об этом с открытой страницы.
func TestEveryPictureOnTheSiteIsCopiedByTheBuild(t *testing.T) {
	page, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href|content)="shots/([^"]+)"`).FindAllSubmatch(page, -1)
	if len(refs) == 0 {
		t.Fatal("на странице нет ни одного снимка — разметка поменялась, а тест нет")
	}
	for _, m := range refs {
		name := string(m[1])
		if ext := filepath.Ext(name); ext != ".png" && ext != ".svg" {
			t.Errorf("%s: сборка копирует только .png и .svg", name)
		}
		if _, err := os.Stat(filepath.Join("..", "docs", "shots", name)); err != nil {
			t.Errorf("shots/%s: в docs/shots такого файла нет", name)
		}
	}
}

// TestSiteNamesNoVersion: ссылки на сайте ведут на список выпусков, а не на
// конкретный. Номер версии на странице устарел бы со следующей альфой, и
// сайт начал бы предлагать старую сборку.
func TestSiteNamesNoVersion(t *testing.T) {
	page, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	if v := regexp.MustCompile(`v\d+\.\d+\.\d+`).Find(page); v != nil {
		t.Errorf("на странице номер версии %s", v)
	}
}
