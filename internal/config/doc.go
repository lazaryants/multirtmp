package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Doc — config.json для программной правки (веб-интерфейс, раздел
// Destinations). Секции, которые UI не трогает (ingest/buffer/api/log/notify),
// хранятся как сырой JSON и переписываются байт в байт без изменений —
// значит, читать их отдельным типизированным структом не нужно, и заодно не
// теряются значения, которые оператор мог вписать вручную сверх того, что
// понимает Resolved (например, поля с умолчаниями, оставленные пустыми в
// файле, не заполняются автоматически обратно). Единственная секция, которую
// Doc понимает по-настоящему — outputs: она уже была структурой для
// программной генерации (POST/PUT/DELETE /outputs), а не просто зоной для
// ручной правки.
//
// Комментарии `//` при этом всё равно теряются в любой секции после первого
// же Save (JSON не умеет их хранить) — см. EnsureBackup.
type Doc struct {
	Ingest  json.RawMessage `json:"ingest,omitempty"`
	Buffer  json.RawMessage `json:"buffer,omitempty"`
	API     json.RawMessage `json:"api,omitempty"`
	Log     json.RawMessage `json:"log,omitempty"`
	Notify  json.RawMessage `json:"notify,omitempty"`
	Outputs []OutputConfig  `json:"outputs"`
}

// LoadDoc читает и разбирает config.json так же, как Load (те же
// `//`-комментарии снимаются перед парсингом), но без применения умолчаний и
// валидации — этот путь только для round-trip чтения/записи под управлением
// веб-интерфейса. Валидность конкретных выходов при добавлении/правке через
// API проверяется отдельно, в момент мутации.
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
