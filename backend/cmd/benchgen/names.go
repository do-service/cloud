package main

import (
	"fmt"
	"math"
	"math/rand/v2"
	"strconv"
	"strings"
)

// The vocabularies mix Cyrillic and Latin on purpose: name_key sorts
// through an ICU collation, and its cost (and the index size) depends on
// the script and length of real names, not on "file_000123".
var (
	dirWords = []string{
		"Документы", "Отчёты", "Фото", "Проекты", "Архив", "Договоры", "Счета",
		"Презентации", "Сканы", "Бухгалтерия", "Входящие", "Черновики", "Отпуск",
		"Семья", "Работа", "Разное", "Шаблоны", "Клиенты", "Протоколы", "Учёба",
		"Ёлка", "Поставщики", "Закупки", "Кадры", "Юристы",
		"Projects", "Photos", "Backup", "Design", "Invoices", "Drafts", "Reports",
		"Music", "Shared", "src", "assets", "docs", "old", "misc", "Clients",
		"Marketing", "Research", "Screenshots", "Downloads",
	}
	fileStems = []string{
		"Отчёт", "Договор", "Счёт", "Акт сверки", "Протокол", "Презентация", "Смета",
		"План", "Заявление", "Резюме", "Справка", "Приказ", "Письмо", "Заметки",
		"Инструкция", "Таблица", "Коммерческое предложение", "Техзадание",
		"report", "invoice", "contract", "notes", "budget", "draft", "summary",
		"specification", "meeting", "roadmap", "README", "changelog",
	}
	months = []string{
		"январь", "февраль", "март", "апрель", "май", "июнь", "июль", "август",
		"сентябрь", "октябрь", "ноябрь", "декабрь",
	}
)

// fileType is an extension with its MIME type, how common it is and the
// median and spread of its size: photos and videos dominate the bytes,
// office documents the count.
type fileType struct {
	ext    string
	mime   string
	weight int
	median float64 // bytes
	sigma  float64 // of the size's natural log
	photo  bool
}

var fileTypes = []fileType{
	{"docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", 14, 40 << 10, 1.0, false},
	{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", 10, 30 << 10, 1.2, false},
	{"pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation", 3, 3 << 20, 1.0, false},
	{"pdf", "application/pdf", 14, 400 << 10, 1.3, false},
	{"txt", "text/plain", 6, 3 << 10, 1.5, false},
	{"md", "text/markdown", 3, 4 << 10, 1.2, false},
	{"csv", "text/csv", 3, 50 << 10, 2.0, false},
	{"odt", "application/vnd.oasis.opendocument.text", 2, 30 << 10, 1.0, false},
	{"jpg", "image/jpeg", 30, 3 << 20, 0.6, true},
	{"png", "image/png", 8, 400 << 10, 1.2, false},
	{"heic", "image/heic", 4, 2 << 20, 0.5, true},
	{"mp4", "video/mp4", 1, 150 << 20, 1.5, false},
	{"zip", "application/zip", 2, 20 << 20, 1.8, false},
}

var fileTypeTotal = func() int {
	n := 0
	for _, t := range fileTypes {
		n += t.weight
	}
	return n
}()

func pickFileType(rng *rand.Rand) fileType {
	n := rng.IntN(fileTypeTotal)
	for _, t := range fileTypes {
		if n < t.weight {
			return t
		}
		n -= t.weight
	}
	return fileTypes[len(fileTypes)-1]
}

// fileSize is log-normal around the type's median: most files are small,
// a few are huge, as on any real share.
func fileSize(rng *rand.Rand, t fileType) int64 {
	return int64(t.median * math.Exp(rng.NormFloat64()*t.sigma))
}

func year(rng *rand.Rand) int { return 2019 + rng.IntN(8) }

func dirName(rng *rand.Rand) string {
	w := dirWords[rng.IntN(len(dirWords))]
	switch rng.IntN(6) {
	case 0:
		return w + " " + strconv.Itoa(year(rng))
	case 1:
		return fmt.Sprintf("%s — %s", w, months[rng.IntN(len(months))])
	case 2:
		return fmt.Sprintf("%02d %s", rng.IntN(20)+1, w)
	}
	return w
}

func fileName(rng *rand.Rand, t fileType) string {
	var stem string
	if t.photo && rng.IntN(3) > 0 {
		switch rng.IntN(3) {
		case 0:
			stem = fmt.Sprintf("IMG_%04d", rng.IntN(10000))
		case 1:
			stem = fmt.Sprintf("DSC%05d", rng.IntN(100000))
		default:
			stem = fmt.Sprintf("Фото %02d.%02d.%d %02d-%02d", rng.IntN(28)+1, rng.IntN(12)+1, year(rng), rng.IntN(24), rng.IntN(60))
		}
		return stem + "." + t.ext
	}
	s := fileStems[rng.IntN(len(fileStems))]
	switch rng.IntN(7) {
	case 0:
		stem = s + " " + strconv.Itoa(rng.IntN(200)+1)
	case 1:
		stem = fmt.Sprintf("%s за %s %d", s, months[rng.IntN(len(months))], year(rng))
	case 2:
		stem = fmt.Sprintf("%s_%d-%02d-%02d", strings.ReplaceAll(s, " ", "_"), year(rng), rng.IntN(12)+1, rng.IntN(28)+1)
	case 3:
		stem = fmt.Sprintf("Скан %02d.%02d.%d", rng.IntN(28)+1, rng.IntN(12)+1, year(rng))
	case 4:
		stem = s + " (финал)"
	case 5:
		stem = fmt.Sprintf("%s v%d", s, rng.IntN(9)+1)
	default:
		stem = s
	}
	return stem + "." + t.ext
}

// dedupe makes name unique among names already taken in its folder the
// way a desktop file manager would, "name (2).ext", so the unique index on
// (parent_id, name_key) holds without retries against the database.
func dedupe(name string, taken func(string) bool) string {
	if !taken(name) {
		return name
	}
	stem, ext := name, ""
	if i := strings.LastIndexByte(name, '.'); i > 0 {
		stem, ext = name[:i], name[i:]
	}
	for k := 2; ; k++ {
		if c := fmt.Sprintf("%s (%d)%s", stem, k, ext); !taken(c) {
			return c
		}
	}
}
