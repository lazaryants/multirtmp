package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// knownSectionOrder — порядок, в котором известные секции пишутся обратно в
// файл (тот же порядок, что и в sampleConfig) — чисто косметика для
// читаемости при ручном редактировании, на сохранность данных не влияет.
// Секции, не входящие в этот список (в т.ч. любые незнакомые этой версии
// multirtmp), пишутся следом, отсортированные по имени для детерминизма.
var knownSectionOrder = []string{"ingest", "buffer", "api", "log", "notify", "preview"}

// Doc — config.json для программной правки (веб-интерфейс, раздел
// Destinations). ЕДИНСТВЕННАЯ секция, которую Doc разбирает по-настоящему —
// outputs; она уже была структурой для программной генерации (POST/PUT/
// DELETE /outputs). Все остальные секции — ЛЮБЫЕ, не только перечисленные в
// knownSectionOrder — хранятся как сырой JSON в extra и переписываются без
// изменений.
//
// Это принципиально важно: более ранняя версия объявляла отдельное
// json.RawMessage-поле на каждую ЗАРАНЕЕ ИЗВЕСТНУЮ секцию (ingest/buffer/
// api/log/notify/preview) — но по плану должны были сохраняться байтами
// ЛЮБЫЕ незнакомые секции, а не только эти шесть. Секция, добавленная в файл
// вручную и не входящая в этот список (например, будущим полем конфига,
// которое эта версия ещё не знает, или сторонним инструментом), у структуры
// с именованными полями просто не имела бы поля для себя и терялась бы при
// каждой Save — план "сохраняем неизвестное сырыми байтами" на деле не
// выполнялся для по-настоящему неизвестного. Custom (Un)MarshalJSON ниже
// перехватывает вообще все ключи верхнего уровня в map, так что неважно,
// знает ли эта версия программы о секции — раз она была в файле, она
// останется в файле после любой Save.
//
// Комментарии `//` при этом всё равно теряются в любой секции после первого
// же Save (JSON не умеет их хранить) — см. EnsureBackup.
type Doc struct {
	Outputs []OutputConfig

	extra map[string]json.RawMessage // все секции верхнего уровня, кроме "outputs", как есть
}

func (d *Doc) UnmarshalJSON(data []byte) error {
	raw := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	if outputsRaw, ok := raw["outputs"]; ok {
		if err := json.Unmarshal(outputsRaw, &d.Outputs); err != nil {
			return fmt.Errorf("outputs: %w", err)
		}
	} else {
		d.Outputs = nil
	}
	delete(raw, "outputs")

	d.extra = raw

	return nil
}

func (d *Doc) MarshalJSON() ([]byte, error) {
	remaining := make(map[string]json.RawMessage, len(d.extra))
	for k, v := range d.extra {
		remaining[k] = v
	}

	var buf bytes.Buffer
	buf.WriteByte('{')

	first := true
	writeField := func(key string, raw json.RawMessage) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false

		keyJSON, err := json.Marshal(key)
		if err != nil {
			return err
		}
		buf.Write(keyJSON)
		buf.WriteByte(':')
		buf.Write(raw)

		return nil
	}

	for _, key := range knownSectionOrder {
		if raw, ok := remaining[key]; ok {
			if err := writeField(key, raw); err != nil {
				return nil, err
			}
			delete(remaining, key)
		}
	}

	// Всё, что осталось — секции, не входящие в knownSectionOrder (в т.ч.
	// незнакомые этой версии программы). Сортировка — только для
	// детерминированного порядка в файле, не для отбора.
	leftoverKeys := make([]string, 0, len(remaining))
	for k := range remaining {
		leftoverKeys = append(leftoverKeys, k)
	}
	sort.Strings(leftoverKeys)
	for _, k := range leftoverKeys {
		if err := writeField(k, remaining[k]); err != nil {
			return nil, err
		}
	}

	outputsJSON, err := json.Marshal(d.Outputs)
	if err != nil {
		return nil, err
	}
	if err := writeField("outputs", outputsJSON); err != nil {
		return nil, err
	}

	buf.WriteByte('}')

	return buf.Bytes(), nil
}

// LoadDoc читает и разбирает config.json так же, как Load (те же
// `//`-комментарии снимаются перед парсингом), но без применения умолчаний и
// валидации — этот путь только для round-trip чтения/записи под управлением
// веб-интерфейса. Валидность конкретных выходов при добавлении/правке через
// API проверяется отдельно, в момент мутации.
//
// Вызывается заново перед КАЖДОЙ записью (см. internal/api/outputs_store.go)
// — не кешируется между вызовами. Кеш, живущий дольше одной операции, был
// первопричиной бага с потерей секции preview: если между двумя вызовами API
// оператор вручную поправил файл (или его поправил любой другой процесс),
// закешированная в памяти версия ничего об этом не знает и следующая же
// запись стирает ручную правку, даже если сама эта запись — что-то
// совершенно не связанное (например, простой клик Start/Stop).
func LoadDoc(path string) (*Doc, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: failed to read %s: %w", path, err)
	}

	stripped := stripLineComments(raw)

	var d Doc
	if err := json.Unmarshal(stripped, &d); err != nil {
		return nil, fmt.Errorf("config: %s", describeJSONError(stripped, err))
	}

	return &d, nil
}

// Save пишет d в path атомарно: во временный файл в той же директории (та же
// файловая система — переименование гарантированно атомарно), потом
// переименовывает поверх path. Так конфиг не может оказаться наполовину
// записанным при сбое посреди записи.
func (d *Doc) Save(path string) error {
	data, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("config: failed to encode: %w", err)
	}
	data = append(data, '\n')

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("config: failed to write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("config: failed to replace %s: %w", path, err)
	}

	return nil
}

// EnsureBackup делает разовую копию path в path+".bak", если такой копии ещё
// нет — подстраховка перед первой программной записью через веб-интерфейс,
// которая безвозвратно снимает `//`-комментарии из файла (см. Doc). Повторные
// вызовы (на каждый следующий запуск процесса) не трогают уже существующий
// .bak — это снимок конфига "как было настроено руками", не текущего
// состояния.
func EnsureBackup(path string) error {
	bak := path + ".bak"
	if _, err := os.Stat(bak); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("config: failed to check %s: %w", bak, err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: failed to read %s for backup: %w", path, err)
	}

	if err := os.WriteFile(bak, data, 0o644); err != nil {
		return fmt.Errorf("config: failed to write backup %s: %w", bak, err)
	}

	return nil
}
