from django.urls import re_path

from brood.server.projects import views

urlpatterns = [
    re_path(r"^projects(?:/(?P<name>[^/]+))?$", views.ProjectView.as_view()),
]
