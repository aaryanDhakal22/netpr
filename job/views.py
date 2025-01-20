from django.shortcuts import render

# Create your views here.
def homepage(req):
    return render(req,"job/homepage.html")


def print_job(req):
    return render(req,"job/success.html")