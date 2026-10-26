import { describe, expect, it } from "vitest";
import { fileType, typeLabel } from "./fileTypes";

const file = (name: string, mime?: string) => ({ kind: "file" as const, name, mime });

describe("fileType", () => {
  it("tells office formats apart, by MIME or by extension", () => {
    expect(fileType(file("a.docx", "application/vnd.openxmlformats-officedocument.wordprocessingml.document"))).toBe("document");
    expect(fileType(file("a.odt", "application/octet-stream"))).toBe("document");
    expect(fileType(file("a.xlsx", "application/zip"))).toBe("spreadsheet");
    expect(fileType(file("a.ods"))).toBe("spreadsheet");
    expect(fileType(file("a.pptx", "application/vnd.openxmlformats-officedocument.presentationml.presentation"))).toBe("presentation");
    expect(fileType(file("a.odp"))).toBe("presentation");
  });

  it("goes by MIME family, then by extension, then gives up", () => {
    expect(fileType({ kind: "dir", name: "Docs" })).toBe("folder");
    expect(fileType(file("photo", "image/jpeg"))).toBe("image");
    expect(fileType(file("a.pdf", "application/pdf"))).toBe("pdf");
    expect(fileType(file("notes.md", "text/markdown; charset=utf-8"))).toBe("text");
    expect(fileType(file("a.zip", "application/zip"))).toBe("archive");
    expect(fileType(file("a.tar"))).toBe("archive");
    expect(fileType(file("a.bin", "application/octet-stream"))).toBe("other");
  });
});

describe("typeLabel", () => {
  it("names the type with the extension, without the raw MIME", () => {
    expect(typeLabel(file("Отчёт.pdf", "application/pdf"))).toBe("Документ PDF");
    expect(typeLabel(file("закат.png", "image/png"))).toBe("Изображение PNG");
    expect(typeLabel(file("план.docx"))).toBe("Документ DOCX");
    expect(typeLabel(file("data.bin", "application/octet-stream"))).toBe("Файл BIN");
    expect(typeLabel(file("Makefile"))).toBe("Файл");
    expect(typeLabel({ kind: "dir", name: "Docs.old" })).toBe("Папка");
  });
});
