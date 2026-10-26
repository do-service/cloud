import { Button, Stack, Text, Title } from "@mantine/core";
import { Link, useLocation } from "react-router-dom";
import { t } from "../i18n";

export default function NotFoundPage() {
  const { pathname } = useLocation();
  return (
    <Stack align="center" gap="sm" py="xl" ta="center">
      <Title order={2}>{t("notFound.title")}</Title>
      <Text c="dimmed">{t("notFound.description", { path: pathname })}</Text>
      <Button component={Link} to="/" variant="light">
        {t("common.toHome")}
      </Button>
    </Stack>
  );
}
