import fs from 'node:fs/promises'
import path from 'node:path'

const LOCALES_DIR = path.resolve('src/i18n/locales')

function stableStringify(obj) {
  return JSON.stringify(obj, null, 2) + '\n'
}

const newKeys = {
  en: {
    'Copy all reference image URLs': 'Copy all reference image URLs',
  },
  zh: {
    'Copy all reference image URLs': '一键复制所有参考图链接',
  },
  'zh-TW': {
    'Copy all reference image URLs': '一鍵複製所有參考圖連結',
  },
  fr: {
    'Copy all reference image URLs': 'Copier toutes les URL des images de référence',
  },
  ja: {
    'Copy all reference image URLs': '参照画像の URL をすべてコピー',
  },
  ru: {
    'Copy all reference image URLs': 'Скопировать все URL референсных изображений',
  },
  vi: {
    'Copy all reference image URLs': 'Sao chép tất cả URL ảnh tham chiếu',
  },
}

async function main() {
  let totalAdded = 0

  for (const [locale, trans] of Object.entries(newKeys)) {
    const filePath = path.join(LOCALES_DIR, `${locale}.json`)
    const json = JSON.parse(await fs.readFile(filePath, 'utf8'))

    let count = 0
    for (const [key, value] of Object.entries(trans)) {
      if (!Object.prototype.hasOwnProperty.call(json.translation, key)) {
        json.translation[key] = value
        count++
      } else if (json.translation[key] !== value) {
        json.translation[key] = value
        count++
      }
    }

    if (count > 0) {
      json.translation = Object.fromEntries(
        Object.entries(json.translation).sort(([a], [b]) => a.localeCompare(b))
      )
      await fs.writeFile(filePath, stableStringify(json), 'utf8')
    }

    console.log(`${locale}: ${count} translations applied`)
    totalAdded += count
  }

  console.log(`\nTotal: ${totalAdded} translations applied`)
}

main().catch((err) => {
  console.error(err)
  process.exitCode = 1
})
