// Печать таблиц. Единственное место, где считаются ширины колонок.
package render

import "strings"

// Table — строки-строки -> выровненные строки. Последняя колонка не
// добивается пробелами, иначе хвост таблицы тянет за собой мусорный отступ.
func Table(rows [][]string) []string {
	if len(rows) == 0 {
		return nil
	}
	cols := 0
	for _, r := range rows {
		if len(r) > cols {
			cols = len(r)
		}
	}
	width := make([]int, cols-1)
	for _, r := range rows {
		for i := 0; i < len(r) && i < len(width); i++ {
			if len(r[i]) > width[i] {
				width[i] = len(r[i])
			}
		}
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		cells := make([]string, len(r))
		copy(cells, r)
		for j := 0; j < len(r) && j < len(width); j++ {
			cells[j] = r[j] + strings.Repeat(" ", width[j]-len(r[j]))
		}
		out[i] = strings.TrimRight(strings.Join(cells, "  "), " ")
	}
	return out
}
